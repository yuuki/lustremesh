// SPDX-License-Identifier: Apache-2.0

//go:build linux

package probe

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yuuki/lustremesh/internal/target"
)

const testInstance = "ffff8f0b3c5e2000"

// fakeTree is a directory laid out like the Lustre parameter trees.
type fakeTree struct {
	t   *testing.T
	dir string
	cfg Config
}

func newFakeTree(t *testing.T) *fakeTree {
	t.Helper()
	dir := t.TempDir()
	return &fakeTree{t: t, dir: dir, cfg: Config{
		SysfsRoot:           filepath.Join(dir, "sys"),
		ProcfsRoot:          filepath.Join(dir, "proc"),
		DebugfsRoot:         filepath.Join(dir, "debug"),
		ContendedRatio:      0.1,
		ContendedFloorNanos: int64(50 * time.Microsecond),
	}}
}

func (ft *fakeTree) write(path, content string) {
	ft.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		ft.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		ft.t.Fatal(err)
	}
}

// device adds an import with its kbytesfree and conn_uuid files.
func (ft *fakeTree) device(name, nid string) target.Device {
	ft.t.Helper()
	d, err := target.ParseDeviceName(name)
	if err != nil {
		ft.t.Fatal(err)
	}
	dir := filepath.Join(ft.cfg.SysfsRoot, d.ClientType(), name)
	ft.write(filepath.Join(dir, "kbytesfree"), "123456\n")
	uuid := "ost_conn_uuid"
	if d.Kind == target.KindMDT {
		uuid = "mds_conn_uuid"
	}
	ft.write(filepath.Join(dir, uuid), nid+"\n")
	d.ServerNID = nid
	return d
}

func newTestProber(t *testing.T, cfg Config) *Prober {
	t.Helper()
	runtime.LockOSThread()
	p := NewProber(cfg)
	t.Cleanup(func() {
		p.Close()
		runtime.UnlockOSThread()
	})
	return p
}

func TestRPC(t *testing.T) {
	ft := newFakeTree(t)
	dev := ft.device("testfs-OST0000-osc-"+testInstance, "10.0.0.2@tcp")
	p := newTestProber(t, ft.cfg)

	r := p.RPC(dev)
	if r.Result != ResultOK || r.Nanos == nil || r.RPCSent != RPCSentUnknown {
		t.Fatalf("got %+v", r)
	}
}

func TestRPCCountersTellCachedFromSent(t *testing.T) {
	ft := newFakeTree(t)
	dev := ft.device("testfs-OST0000-osc-"+testInstance, "10.0.0.2@tcp")
	stats := filepath.Join(ft.cfg.DebugfsRoot, "osc", dev.Name, "stats")
	ft.write(stats, "ost_statfs 5 samples [usec] 1 1 5\n")
	p := newTestProber(t, ft.cfg)

	// The counter does not move: the cache answered.
	r := p.RPC(dev)
	if r.Result != ResultCached || r.RPCSent != RPCSentNo {
		t.Fatalf("unchanged counter: %+v", r)
	}

	// An MDT uses its own counter name.
	mdt := ft.device("testfs-MDT0000-mdc-"+testInstance, "10.0.0.1@tcp")
	ft.write(filepath.Join(ft.cfg.DebugfsRoot, "mdc", mdt.Name, "stats"), "ost_statfs 5 samples\n")
	if r := p.RPC(mdt); r.Result != ResultOK || r.RPCSent != RPCSentUnknown {
		t.Fatalf("MDT without mds_statfs: %+v", r)
	}
}

func TestRPCFloor(t *testing.T) {
	ft := newFakeTree(t)
	dev := ft.device("testfs-OST0000-osc-"+testInstance, "10.0.0.2@tcp")
	ft.cfg.RPCFloorNanos = int64(time.Hour)
	p := newTestProber(t, ft.cfg)
	if r := p.RPC(dev); r.Result != ResultCached || r.RPCSent != RPCSentUnknown {
		t.Fatalf("got %+v", r)
	}
}

func TestRPCFailures(t *testing.T) {
	ft := newFakeTree(t)
	dev := ft.device("testfs-OST0000-osc-"+testInstance, "10.0.0.2@tcp")
	p := newTestProber(t, ft.cfg)
	path := filepath.Join(ft.cfg.SysfsRoot, "osc", dev.Name, "kbytesfree")

	ft.write(path, "not a number\n")
	if r := p.RPC(dev); r.Result != ResultError {
		t.Errorf("garbage content: %+v", r)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if r := p.RPC(dev); r.Result != ResultError || r.Errno != "ENOENT" {
		t.Errorf("missing file: %+v", r)
	}
	// A directory fails the read itself (EISDIR): an error, with its errno.
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if r := p.RPC(dev); r.Result != ResultError || r.Errno != "EISDIR" {
		t.Errorf("directory: %+v", r)
	}
}

func TestImportState(t *testing.T) {
	ft := newFakeTree(t)
	dev := ft.device("testfs-OST0000-osc-"+testInstance, "10.0.0.2@tcp")
	p := newTestProber(t, ft.cfg)
	if got := p.ImportState(dev); got != StateUnknown {
		t.Errorf("no state file: %q", got)
	}
	// Lustre 2.17 keeps the file in debugfs; procfs wins when both exist.
	ft.write(filepath.Join(ft.cfg.DebugfsRoot, "osc", dev.Name, "state"), "current_state: IDLE\n")
	if got := p.ImportState(dev); got != "IDLE" {
		t.Errorf("debugfs: %q", got)
	}
	ft.write(filepath.Join(ft.cfg.ProcfsRoot, "osc", dev.Name, "state"), "current_state: FULL\n")
	if got := p.ImportState(dev); got != "FULL" {
		t.Errorf("procfs: %q", got)
	}
}

func TestDiscover(t *testing.T) {
	ft := newFakeTree(t)
	ft.device("testfs-OST0001-osc-"+testInstance, "10.0.0.2@tcp")
	ft.device("testfs-MDT0000-mdc-"+testInstance, "10.0.0.1@tcp")
	// A server-side import, a stray file, and a device without conn_uuid.
	if err := os.MkdirAll(filepath.Join(ft.cfg.SysfsRoot, "osc", "testfs-OST0001-osc-MDT0000"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ft.cfg.SysfsRoot, "osc", "testfs-OST0002-osc-"+testInstance), 0o755); err != nil {
		t.Fatal(err)
	}
	// An MDT name under osc/ is not an OSC import.
	if err := os.MkdirAll(filepath.Join(ft.cfg.SysfsRoot, "osc", "testfs-MDT0003-mdc-"+testInstance), 0o755); err != nil {
		t.Fatal(err)
	}

	devs, skipped, err := Discover(ft.cfg)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, d := range devs {
		got[d.Label()] = d.ServerNID
	}
	want := map[string]string{"testfs-MDT0000": "10.0.0.1@tcp", "testfs-OST0001": "10.0.0.2@tcp", "testfs-OST0002": "unknown"}
	if len(got) != len(want) {
		t.Fatalf("devices: %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: NID %q, want %q", k, got[k], v)
		}
	}
	if len(skipped) != 2 {
		t.Errorf("skipped: %v", skipped)
	}

	if _, _, err := Discover(Config{SysfsRoot: filepath.Join(ft.dir, "nowhere")}); err == nil {
		t.Error("no Lustre directory: no error")
	}
}

// dataRoot prepares <root>/v1/ost/<index>/ and returns the spec of a file in it.
func dataRoot(t *testing.T, index string) DataSpec {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "v1", "ost", index), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := DataSpec{Root: root, IndexHex: index, ClientID: "node-a001", Direct: true}
	// The temporary directory may be on a file system without O_DIRECT.
	probeFile := filepath.Join(root, "direct-check")
	fd, err := syscall.Open(probeFile, syscall.O_RDWR|syscall.O_CREAT|syscall.O_DIRECT, 0o600)
	if errors.Is(err, syscall.EINVAL) {
		spec.Direct = false
	} else if err == nil {
		_ = syscall.Close(fd)
	}
	_ = os.Remove(probeFile)
	return spec
}

func TestDataProbeRoundTrip(t *testing.T) {
	spec := dataRoot(t, "0003")
	if err := os.WriteFile(spec.Path(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p := newTestProber(t, Config{ContendedRatio: 1, ContendedFloorNanos: int64(time.Hour)})

	f, r := p.OpenData(spec)
	if r.Result != ResultOK || f == nil || r.Nanos == nil {
		t.Fatalf("open: %+v", r)
	}
	// A file that was not verified is never written to.
	if r := f.Write(); r.Result != ResultError || r.Nanos != nil {
		t.Fatalf("write before verify: %+v", r)
	}
	if r := f.Verify("", false, 3); r.Result != ResultOK {
		t.Fatalf("verify: %+v", r)
	}
	if r := f.Write(); r.Result != ResultOK || r.Nanos == nil {
		t.Fatalf("write: %+v", r)
	}
	if r := f.Read(); r.Result != ResultOK || r.Nanos == nil {
		t.Fatalf("read: %+v", r)
	}
	if r := f.Close(); r.Result != ResultOK {
		t.Fatalf("close: %+v", r)
	}
	data, err := os.ReadFile(spec.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != os.Getpagesize() || !strings.HasPrefix(string(data), "lustremesh v1\nclient=node-a001\nseq=") {
		t.Errorf("file holds %d bytes starting %q", len(data), data[:min(len(data), 40)])
	}
}

func TestReadDetectsMismatch(t *testing.T) {
	spec := dataRoot(t, "0003")
	if err := os.WriteFile(spec.Path(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p := newTestProber(t, Config{ContendedRatio: 1, ContendedFloorNanos: int64(time.Hour)})
	f, r := p.OpenData(spec)
	if f == nil {
		t.Fatalf("open: %+v", r)
	}
	defer f.Close()
	if r := f.Verify("", false, 3); r.Result != ResultOK {
		t.Fatalf("verify: %+v", r)
	}
	if r := f.Write(); r.Result != ResultOK {
		t.Fatalf("write: %+v", r)
	}
	// Something overwrites the page between the write and the read.
	other := make([]byte, os.Getpagesize())
	if err := os.WriteFile(spec.Path(), other, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := f.Read(); r.Result != ResultMismatch || strings.Contains(r.Detail, "client ID") {
		t.Fatalf("garbage: %+v", r)
	}
	// Another probe's page is told apart from corruption.
	BuildPayload(other, "node-a001", 99, time.Now())
	if err := os.WriteFile(spec.Path(), other, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := f.Read(); r.Result != ResultMismatch || !strings.Contains(r.Detail, "client ID") {
		t.Fatalf("another probe's page: %+v", r)
	}
}

func TestClassifyRPCError(t *testing.T) {
	tests := map[error]Result{
		syscall.EAGAIN:    ResultDisconnected,
		syscall.ESHUTDOWN: ResultInactive,
		syscall.ENODATA:   ResultInactive,
		syscall.EIO:       ResultError,
		syscall.ENODEV:    ResultError,
		errors.New("x"):   ResultError,
	}
	for err, want := range tests {
		if got := classifyRPCError(err); got != want {
			t.Errorf("%v: %s, want %s", err, got, want)
		}
	}
}

func TestOpenDataRefusesUnsuitableFiles(t *testing.T) {
	p := newTestProber(t, Config{})
	t.Run("missing file", func(t *testing.T) {
		spec := dataRoot(t, "0003")
		if f, r := p.OpenData(spec); f != nil || r.Result != ResultUnprepared || r.Errno != "ENOENT" {
			t.Fatalf("got %+v", r)
		}
	})
	t.Run("missing directory", func(t *testing.T) {
		spec := dataRoot(t, "0003")
		spec.IndexHex = "0004"
		if f, r := p.OpenData(spec); f != nil || r.Result != ResultUnprepared {
			t.Fatalf("got %+v", r)
		}
	})
	t.Run("symbolic link", func(t *testing.T) {
		spec := dataRoot(t, "0003")
		victim := filepath.Join(t.TempDir(), "victim")
		if err := os.WriteFile(victim, []byte("precious"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victim, spec.Path()); err != nil {
			t.Fatal(err)
		}
		if f, r := p.OpenData(spec); f != nil || r.Result != ResultUnprepared || r.Errno != "ELOOP" {
			t.Fatalf("got %+v", r)
		}
	})
	t.Run("symbolic link in the path", func(t *testing.T) {
		spec := dataRoot(t, "0003")
		elsewhere := t.TempDir()
		if err := os.WriteFile(filepath.Join(elsewhere, spec.ClientID), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(spec.Root, "v1", "ost", "0003")
		if err := os.Remove(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, dir); err != nil {
			t.Fatal(err)
		}
		if f, r := p.OpenData(spec); f != nil || r.Result != ResultUnprepared {
			t.Fatalf("got %+v", r)
		}
	})
	t.Run("hard link", func(t *testing.T) {
		spec := dataRoot(t, "0003")
		victim := filepath.Join(spec.Root, "victim")
		if err := os.WriteFile(victim, []byte("precious"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(victim, spec.Path()); err != nil {
			t.Fatal(err)
		}
		f, r := p.OpenData(spec)
		if f == nil {
			t.Fatalf("open: %+v", r)
		}
		defer f.Close()
		if r := f.Verify("", false, 3); r.Result != ResultUnprepared || !strings.Contains(r.Detail, "hard links") {
			t.Fatalf("verify: %+v", r)
		}
		if r := f.Write(); r.Result != ResultError {
			t.Fatalf("write: %+v", r)
		}
		if data, _ := os.ReadFile(victim); string(data) != "precious" {
			t.Fatalf("the victim was written to: %q", data)
		}
	})
	t.Run("directory", func(t *testing.T) {
		spec := dataRoot(t, "0003")
		if err := os.Mkdir(spec.Path(), 0o755); err != nil {
			t.Fatal(err)
		}
		if f, r := p.OpenData(spec); f != nil || r.Result.Answered() {
			t.Fatalf("got %+v", r)
		}
	})
	t.Run("invalid names", func(t *testing.T) {
		spec := dataRoot(t, "0003")
		for _, bad := range []DataSpec{
			{Root: spec.Root, IndexHex: "0003", ClientID: "../../etc"},
			{Root: spec.Root, IndexHex: "../x", ClientID: "node-a001"},
			{Root: spec.Root, IndexHex: "0003", ClientID: ""},
		} {
			if f, r := p.OpenData(bad); f != nil || r.Result != ResultError {
				t.Fatalf("%+v: got %+v", bad, r)
			}
		}
	})
}

// fakeLfs writes a script that stands for "lfs getstripe".
func fakeLfs(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lfs")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// layout is a script body that prints a plain one-stripe layout on an OST.
func layout(ost string) string {
	return `printf '%s\nlmm_stripe_count:  1\nlmm_pattern:       raid0\nlmm_stripe_offset: ` + ost + `\n' "$2"`
}

func TestVerifyOST(t *testing.T) {
	spec := dataRoot(t, "0003")
	if err := os.WriteFile(spec.Path(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p := newTestProber(t, Config{})
	f, r := p.OpenData(spec)
	if f == nil {
		t.Fatalf("open: %+v", r)
	}
	defer f.Close()

	tests := []struct {
		name, script string
		want         Result
	}{
		{"right OST", `[ "$1" = getstripe ] && [ -f "$2" ] && ` + layout("3"), ResultOK},
		{"another OST", layout("5"), ResultUnprepared},
		{"only an index", `echo 3`, ResultUnprepared},
		{"composite", `printf 'lcm_entry_count: 2\nlmm_stripe_count: 1\nlmm_pattern: raid0\nlmm_stripe_offset: 3\n'`, ResultUnprepared},
		{"data on the MDT", `printf 'lmm_stripe_count: 1\nlmm_pattern: mdt\nlmm_stripe_offset: 3\n'`, ResultUnprepared},
		{"no output", `true`, ResultUnprepared},
		{"lfs fails", `printf 'not a\nLustre\tfile\033[31m' >&2; exit 1`, ResultUnprepared},
	}
	for _, tc := range tests {
		f.verified = false
		got := f.Verify(fakeLfs(t, tc.script), true, 3)
		if got.Result != tc.want || f.verified != (tc.want == ResultOK) {
			t.Errorf("%s: %+v (verified=%v)", tc.name, got, f.verified)
		}
		if strings.ContainsAny(got.Detail, "\n\t\x1b") {
			t.Errorf("%s: the detail spans lines or has control characters: %q", tc.name, got.Detail)
		}
	}
	if got := f.Verify(filepath.Join(t.TempDir(), "no-such-lfs"), true, 3); got.Result != ResultUnprepared || f.verified {
		t.Errorf("missing lfs: %+v", got)
	}

	// The path is replaced while lfs runs: the answer is about another file.
	swap := fakeLfs(t, `mv "$2" "$2.old" && : > "$2" && `+layout("3"))
	if got := f.Verify(swap, true, 3); got.Result != ResultUnprepared || !strings.Contains(got.Detail, "changed") || f.verified {
		t.Errorf("swapped file: %+v", got)
	}
}

func TestLfsInheritsTheLockFiles(t *testing.T) {
	spec := dataRoot(t, "0003")
	if err := os.WriteFile(spec.Path(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := os.Create(filepath.Join(t.TempDir(), "lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	p := newTestProber(t, Config{})
	p.Inherit = []*os.File{lock}
	f, r := p.OpenData(spec)
	if f == nil {
		t.Fatalf("open: %+v", r)
	}
	defer f.Close()
	// The script succeeds only if descriptor 3 is the inherited lock file.
	script := `[ "$(readlink /proc/$$/fd/3)" = "` + lock.Name() + `" ] && ` + layout("3")
	if got := f.Verify(fakeLfs(t, script), true, 3); got.Result != ResultOK {
		t.Errorf("got %+v", got)
	}
}
