// SPDX-License-Identifier: Apache-2.0

//go:build linux

package probe

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/yuuki/lustremesh/internal/target"
)

// Prober sends probes from one OS thread. The caller locks the goroutine to
// its thread (runtime.LockOSThread) before NewProber and for every call: the
// run-queue wait is read from the calling thread's own schedstat.
type Prober struct {
	cfg     Config
	schedFD int // -1 when the kernel has no schedstat
	small   [64]byte
	// Inherit lists the files a child process (lfs) keeps open, so that
	// the locks they carry outlive this process if the child blocks.
	Inherit []*os.File
}

// NewProber prepares a prober for the calling thread.
func NewProber(cfg Config) *Prober {
	p := &Prober{cfg: cfg, schedFD: -1}
	fd, err := open("/proc/thread-self/schedstat", syscall.O_RDONLY|syscall.O_CLOEXEC)
	if err == nil {
		p.schedFD = fd
	}
	return p
}

// Close releases the prober's descriptor.
func (p *Prober) Close() {
	if p.schedFD >= 0 {
		_ = syscall.Close(p.schedFD)
		p.schedFD = -1
	}
}

func (p *Prober) runDelay() (time.Duration, bool) {
	if p.schedFD < 0 {
		return 0, false
	}
	var buf [96]byte
	n, err := pread(p.schedFD, buf[:], 0)
	if err != nil || n <= 0 {
		return 0, false
	}
	return ParseRunDelay(buf[:n])
}

// timed runs one system call between two clock readings. The run-queue wait
// is read outside the clock. An interrupted call is restarted inside it.
func (p *Prober) timed(call func() (int, error)) (n int, elapsed time.Duration, delay *int64, err error) {
	d0, ok0 := p.runDelay()
	start := time.Now()
	for {
		n, err = call()
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	elapsed = time.Since(start)
	d1, ok1 := p.runDelay()
	if ok0 && ok1 && d1 >= d0 {
		v := int64(d1 - d0)
		delay = &v
	}
	return n, elapsed, delay, err
}

// measured fills r with the duration of a completed call and, when the call
// succeeded, with ResultOK or ResultContended.
func (p *Prober) measured(r *StepResult, elapsed time.Duration, delay *int64, err error) {
	ns := int64(elapsed)
	r.Nanos = &ns
	r.RunDelayNanos = delay
	if err != nil {
		return
	}
	r.Result = ResultOK
	if delay != nil && p.cfg.Contended(elapsed, time.Duration(*delay)) {
		r.Result = ResultContended
		r.Detail = fmt.Sprintf("waited %s for a CPU", time.Duration(*delay))
	}
}

// ImportState reads the state of the device's import, or StateUnknown.
func (p *Prober) ImportState(dev target.Device) string {
	for _, root := range []string{p.cfg.ProcfsRoot, p.cfg.DebugfsRoot} {
		if root == "" {
			continue
		}
		data, err := readSmall(filepath.Join(root, dev.ClientType(), dev.Name, "state"))
		if err == nil {
			return ParseImportState(data)
		}
	}
	return StateUnknown
}

func (p *Prober) statfsCount(dev target.Device) (uint64, bool) {
	if p.cfg.DebugfsRoot == "" {
		return 0, false
	}
	data, err := readSmall(filepath.Join(p.cfg.DebugfsRoot, dev.ClientType(), dev.Name, "stats"))
	if err != nil {
		return 0, false
	}
	name := "ost_statfs"
	if dev.Kind == target.KindMDT {
		name = "mds_statfs"
	}
	return ParseStatCount(data, name)
}

// RPC sends the rpc probe: one read of the device's kbytesfree, which makes
// the client send a statfs RPC to the target unless its one-second cache
// answers. The read does not wait for an import that is known to be
// disconnected: it fails with EAGAIN, reported as ResultDisconnected. It
// does wait for an idle import to reconnect, and so blocks when the server
// of an idle import is down.
func (p *Prober) RPC(dev target.Device) StepResult {
	r := StepResult{Step: StepRPC, Result: ResultError, RPCSent: RPCSentUnknown}
	path := filepath.Join(p.cfg.SysfsRoot, dev.ClientType(), dev.Name, "kbytesfree")

	before, okBefore := p.statfsCount(dev)
	fd, err := open(path, syscall.O_RDONLY|syscall.O_CLOEXEC)
	if err != nil {
		r.Errno = errnoName(err)
		r.Detail = "open " + path + ": " + err.Error()
		return r
	}
	defer func() { _ = syscall.Close(fd) }()

	n, elapsed, delay, err := p.timed(func() (int, error) { return syscall.Pread(fd, p.small[:], 0) })
	p.measured(&r, elapsed, delay, err)
	if err != nil {
		r.Errno = errnoName(err)
		r.Result = classifyRPCError(err)
		if r.Result == ResultError {
			r.Detail = "read " + path + ": " + err.Error()
		}
		return r
	}
	if !ValidKbytes(p.small[:n]) {
		r.Result = ResultError
		r.Detail = path + " did not return a number"
		return r
	}

	after, okAfter := p.statfsCount(dev)
	switch {
	case okBefore && okAfter && after > before:
		r.RPCSent = RPCSentYes
	case okBefore && okAfter:
		r.RPCSent = RPCSentNo
		r.Result = ResultCached
		r.Detail = "answered from the client's statfs cache"
	case p.cfg.RPCFloorNanos > 0 && int64(elapsed) < p.cfg.RPCFloorNanos:
		r.Result = ResultCached
		r.Detail = fmt.Sprintf("faster than --rpc-floor (%s): taken for a cached answer", time.Duration(p.cfg.RPCFloorNanos))
	}
	return r
}

// classifyRPCError maps the error of the kbytesfree read to a result.
func classifyRPCError(err error) Result {
	switch {
	case errors.Is(err, syscall.EAGAIN):
		return ResultDisconnected
	case errors.Is(err, syscall.ESHUTDOWN), errors.Is(err, syscall.ENODATA):
		return ResultInactive
	default:
		return ResultError
	}
}

// DataFile is a client's pinned file on one OST, open for a data probe.
type DataFile struct {
	p        *Prober
	fd       int
	path     string
	clientID string
	id       fileID
	verified bool
	mem      []byte // two pages: the write buffer, then the read buffer
	page     int
}

// OpenData opens the pinned file, timing only the final open. It walks the
// probe root one component at a time and never follows a symbolic link below
// it. A missing file is ResultUnprepared, never created here: creating it
// needs "lfs setstripe -o" (docs/design.md §5.1).
//
// Everything OpenData does goes to the metadata servers only. Whether the
// file may be written to is checked by Verify, which can involve the OST.
func (p *Prober) OpenData(spec DataSpec) (*DataFile, StepResult) {
	r := StepResult{Step: StepOpen, Result: ResultError}
	if !ValidClientID(spec.ClientID) || !validIndexHex(spec.IndexHex) {
		r.Detail = "invalid client ID or OST index"
		return nil, r
	}
	path := spec.Path()

	dirfd, err := open(spec.Root, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC)
	if err != nil {
		return nil, openFailure(r, spec.Root, err)
	}
	walked := spec.Root
	for _, name := range []string{"v1", "ost", spec.IndexHex} {
		walked = filepath.Join(walked, name)
		next, err := openat(dirfd, name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC)
		_ = syscall.Close(dirfd)
		if err != nil {
			return nil, openFailure(r, walked, err)
		}
		dirfd = next
	}
	defer func() { _ = syscall.Close(dirfd) }()

	flags := syscall.O_RDWR | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	if spec.Direct {
		flags |= syscall.O_DIRECT
	}
	fd, elapsed, delay, err := p.timed(func() (int, error) {
		return syscall.Openat(dirfd, spec.ClientID, flags, 0)
	})
	p.measured(&r, elapsed, delay, err)
	if err != nil {
		return nil, openFailure(r, path, err)
	}

	f := &DataFile{p: p, fd: fd, path: path, clientID: spec.ClientID, page: os.Getpagesize()}
	// Anonymous mappings are page aligned, as O_DIRECT requires of the
	// buffer; the offset (0) and the length (one page) are aligned too.
	f.mem, err = syscall.Mmap(-1, 0, 2*f.page, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		_ = syscall.Close(fd)
		r.Result = ResultError
		r.Detail = "mmap: " + err.Error()
		return nil, r
	}
	return f, r
}

func openFailure(r StepResult, path string, err error) StepResult {
	r.Errno = errnoName(err)
	r.Detail = path + ": " + err.Error()
	switch {
	case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ELOOP), errors.Is(err, syscall.ENOTDIR),
		errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		r.Result = ResultUnprepared
	default:
		r.Result = ResultError
	}
	return r
}

// fileID is what identifies a file and tells whether it may be written to.
type fileID struct {
	dev   uint64
	ino   uint64
	mode  uint32
	uid   uint32
	nlink uint64
}

// idMask leaves out the size, the block count and the times on purpose: on
// Lustre those are asked of the OST, and this is not the probe.
const idMask = unix.STATX_TYPE | unix.STATX_MODE | unix.STATX_NLINK | unix.STATX_UID | unix.STATX_INO

// identify reads the identity of an open file (path "") or of a path, whose
// last component is not followed. It prefers statx, which can leave the OST
// alone; a kernel without statx falls back to stat.
func identify(fd int, path string) (fileID, error) {
	dirfd, flags := unix.AT_FDCWD, unix.AT_SYMLINK_NOFOLLOW|unix.AT_STATX_DONT_SYNC
	if path == "" {
		dirfd, flags = fd, unix.AT_EMPTY_PATH|unix.AT_STATX_DONT_SYNC
	}
	var sx unix.Statx_t
	for {
		err := unix.Statx(dirfd, path, flags, idMask, &sx)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err == nil && sx.Mask&idMask == idMask {
			return fileID{
				dev:   unix.Mkdev(sx.Dev_major, sx.Dev_minor),
				ino:   sx.Ino,
				mode:  uint32(sx.Mode),
				uid:   sx.Uid,
				nlink: uint64(sx.Nlink),
			}, nil
		}
		if err != nil && !errors.Is(err, syscall.ENOSYS) {
			return fileID{}, err
		}
		break
	}
	var st syscall.Stat_t
	var err error
	if path == "" {
		err = syscall.Fstat(fd, &st)
	} else {
		err = syscall.Lstat(path, &st)
	}
	if err != nil {
		return fileID{}, err
	}
	return fileID{dev: uint64(st.Dev), ino: st.Ino, mode: st.Mode, uid: st.Uid, nlink: uint64(st.Nlink)}, nil //nolint:unconvert // the field types differ between architectures
}

// unsuitable says why a file must not be written to, or "". The probe
// overwrites the first page, so the file has to be this user's own regular
// file under one name only: a hard link to somebody else's file is refused.
func (id fileID) unsuitable() string {
	switch {
	case id.mode&syscall.S_IFMT != syscall.S_IFREG:
		return "is not a regular file"
	case int(id.uid) != os.Geteuid():
		return fmt.Sprintf("belongs to uid %d, not to this user (uid %d)", id.uid, os.Geteuid())
	case id.nlink != 1:
		return fmt.Sprintf("has %d hard links", id.nlink)
	}
	return ""
}

// Verify decides whether the open file may be written to: it must be this
// user's own regular file with one link and, when checkOST is set, "lfs
// getstripe" must show a plain layout of one stripe on the OST the probe is
// attributed to. Anything else is ResultUnprepared: a probe must overwrite
// nobody's data and must not be sent to another OST than the one it names.
//
// Unlike OpenData, Verify can wait for the OST: reading a file's attributes
// or layout may ask the OST on some Lustre versions.
func (f *DataFile) Verify(lfsPath string, checkOST bool, want uint32) StepResult {
	r := StepResult{Step: StepVerify, Result: ResultUnprepared}
	id, err := identify(f.fd, "")
	if err != nil {
		r.Result = ResultError
		r.Errno = errnoName(err)
		r.Detail = "stat " + f.path + ": " + err.Error()
		return r
	}
	f.id = id
	if why := id.unsuitable(); why != "" {
		r.Detail = f.path + " " + why
		return r
	}
	if !checkOST {
		f.verified = true
		r.Result = ResultOK
		return r
	}

	bin, err := exec.LookPath(lfsPath)
	if err != nil {
		r.Detail = "cannot verify the OST of " + f.path + ": " + err.Error()
		return r
	}
	cmd := exec.CommandContext(context.Background(), bin, "getstripe", f.path)
	cmd.Env = []string{"LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	// lfs may block as this process can. It inherits the target's locks, so
	// that the target stays locked while lfs lives, even if this process is
	// killed meanwhile.
	cmd.ExtraFiles = f.p.Inherit
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, n: 512}
	out, err := cmd.Output()
	if err != nil {
		r.Detail = fmt.Sprintf("%s getstripe %s: %v: %s", bin, f.path, err, OneLine(stderr.Bytes(), 200))
		return r
	}
	got, err := ParsePlainLayout(out)
	if err != nil {
		r.Detail = f.path + ": " + err.Error()
		return r
	}
	// lfs resolved the path again: make sure it looked at the open file.
	if again, err := identify(-1, f.path); err != nil || again.ino != id.ino || again.dev != id.dev {
		r.Detail = f.path + " changed while its OST was verified"
		return r
	}
	if got != want {
		r.Detail = fmt.Sprintf("%s is on OST index %d, not %d", f.path, got, want)
		return r
	}
	f.verified = true
	r.Result = ResultOK
	return r
}

// Write overwrites the first page of the file with a fresh payload. It
// refuses a file that Verify did not accept.
func (f *DataFile) Write() StepResult {
	r := StepResult{Step: StepWrite, Result: ResultError}
	if !f.verified {
		r.Detail = "the file was not verified"
		return r
	}
	var seq [8]byte
	if _, err := rand.Read(seq[:]); err != nil {
		r.Detail = "random sequence number: " + err.Error()
		return r
	}
	wbuf := f.mem[:f.page]
	BuildPayload(wbuf, f.clientID, binary.LittleEndian.Uint64(seq[:]), time.Now())

	n, elapsed, delay, err := f.p.timed(func() (int, error) { return syscall.Pwrite(f.fd, wbuf, 0) })
	f.p.measured(&r, elapsed, delay, err)
	if err != nil {
		r.Errno = errnoName(err)
		r.Detail = "write: " + err.Error()
		return r
	}
	if n != f.page {
		r.Result = ResultError
		r.Detail = fmt.Sprintf("short write: %d of %d bytes", n, f.page)
	}
	return r
}

// Read reads the first page back and compares it with what Write wrote.
func (f *DataFile) Read() StepResult {
	r := StepResult{Step: StepRead, Result: ResultError}
	wbuf, rbuf := f.mem[:f.page], f.mem[f.page:]
	clear(rbuf)

	n, elapsed, delay, err := f.p.timed(func() (int, error) { return syscall.Pread(f.fd, rbuf, 0) })
	f.p.measured(&r, elapsed, delay, err)
	if err != nil {
		r.Errno = errnoName(err)
		r.Detail = "read: " + err.Error()
		return r
	}
	if n != f.page {
		r.Result = ResultError
		r.Detail = fmt.Sprintf("short read: %d of %d bytes", n, f.page)
		return r
	}
	if !bytes.Equal(wbuf, rbuf) {
		r.Result = ResultMismatch
		r.Detail = "the page read back is not the page just written"
		if IsPayload(rbuf) {
			// Not corruption: a whole page of another probe.
			r.Detail = "the page read back was written by another probe: is the client ID used by two clients?"
		}
	}
	return r
}

// Close closes the file. On Lustre this sends a close RPC to the MDS and can
// block like any other step.
func (f *DataFile) Close() StepResult {
	r := StepResult{Step: StepClose, Result: ResultOK}
	if err := syscall.Close(f.fd); err != nil {
		r.Result = ResultError
		r.Errno = errnoName(err)
		r.Detail = "close: " + err.Error()
	}
	f.fd = -1
	if f.mem != nil {
		_ = syscall.Munmap(f.mem)
		f.mem = nil
	}
	return r
}

// Discover lists the client's import devices and the NID each is connected
// to. Names it does not recognize as client imports are returned in skipped.
func Discover(cfg Config) (devs []target.Device, skipped []string, err error) {
	found := false
	for _, typ := range []string{"mdc", "osc"} {
		dir := filepath.Join(cfg.SysfsRoot, typ)
		entries, rerr := os.ReadDir(dir)
		if rerr != nil {
			if errors.Is(rerr, os.ErrNotExist) {
				continue
			}
			return nil, nil, rerr
		}
		found = true
		for _, e := range entries {
			d, perr := target.ParseDeviceName(e.Name())
			if perr != nil || d.ClientType() != typ {
				if len(skipped) < 32 {
					skipped = append(skipped, typ+"/"+e.Name())
				}
				continue
			}
			uuid := "ost_conn_uuid"
			if d.Kind == target.KindMDT {
				uuid = "mds_conn_uuid"
			}
			if data, rerr := readSmall(filepath.Join(dir, e.Name(), uuid)); rerr == nil {
				d.ServerNID = SanitizeNID(data)
			}
			devs = append(devs, d)
		}
	}
	if !found {
		return nil, nil, fmt.Errorf("neither %s/mdc nor %s/osc exists: is a Lustre file system mounted?", cfg.SysfsRoot, cfg.SysfsRoot)
	}
	return devs, skipped, nil
}

func validIndexHex(s string) bool {
	if len(s) < 4 || len(s) > 8 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// readSmall reads a parameter file of at most 64 KiB.
func readSmall(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, 64<<10))
}

func open(path string, flags int) (int, error) {
	for {
		fd, err := syscall.Open(path, flags, 0)
		if !errors.Is(err, syscall.EINTR) {
			return fd, err
		}
	}
}

func openat(dirfd int, name string, flags int) (int, error) {
	for {
		fd, err := syscall.Openat(dirfd, name, flags, 0)
		if !errors.Is(err, syscall.EINTR) {
			return fd, err
		}
	}
}

func pread(fd int, buf []byte, off int64) (int, error) {
	for {
		n, err := syscall.Pread(fd, buf, off)
		if !errors.Is(err, syscall.EINTR) {
			return n, err
		}
	}
}

// limitedWriter keeps the first n bytes written to it.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	keep := p
	if len(keep) > l.n {
		keep = keep[:l.n]
	}
	l.n -= len(keep)
	if len(keep) > 0 {
		if _, err := l.w.Write(keep); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

var errnoNames = map[syscall.Errno]string{
	syscall.EPERM:       "EPERM",
	syscall.ENOENT:      "ENOENT",
	syscall.EINTR:       "EINTR",
	syscall.EIO:         "EIO",
	syscall.ENXIO:       "ENXIO",
	syscall.EAGAIN:      "EAGAIN",
	syscall.ENOMEM:      "ENOMEM",
	syscall.EACCES:      "EACCES",
	syscall.EBUSY:       "EBUSY",
	syscall.ENODEV:      "ENODEV",
	syscall.ENOTDIR:     "ENOTDIR",
	syscall.EISDIR:      "EISDIR",
	syscall.EINVAL:      "EINVAL",
	syscall.EFBIG:       "EFBIG",
	syscall.ENOSPC:      "ENOSPC",
	syscall.EROFS:       "EROFS",
	syscall.ELOOP:       "ELOOP",
	syscall.ENODATA:     "ENODATA",
	syscall.EREMOTEIO:   "EREMOTEIO",
	syscall.ENOTCONN:    "ENOTCONN",
	syscall.ESHUTDOWN:   "ESHUTDOWN",
	syscall.ETIMEDOUT:   "ETIMEDOUT",
	syscall.ESTALE:      "ESTALE",
	syscall.EDQUOT:      "EDQUOT",
	syscall.EINPROGRESS: "EINPROGRESS",
}

func errnoName(err error) string {
	var e syscall.Errno
	if !errors.As(err, &e) {
		return ""
	}
	if name, ok := errnoNames[e]; ok {
		return name
	}
	return fmt.Sprintf("errno %d", int(e))
}
