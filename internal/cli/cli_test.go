// SPDX-License-Identifier: Apache-2.0

//go:build linux

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/yuuki/lustremesh/internal/check"
	"github.com/yuuki/lustremesh/internal/probe"
)

const inst = "ffff8f0b3c5e2000"

// TestMain makes the test binary a lustremesh worker when it is started as
// one: "check" re-executes its own executable with the worker subcommand.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == workerCommand {
		os.Exit(Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func run(args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = Main(args, strings.NewReader(""), &out, &errb)
	return code, out.String(), errb.String()
}

func TestCommands(t *testing.T) {
	if code, out, _ := run("version"); code != ExitOK || !strings.HasPrefix(out, "lustremesh ") {
		t.Errorf("version: %d %q", code, out)
	}
	if code, out, _ := run("help"); code != ExitOK || !strings.Contains(out, "lustremesh check") {
		t.Errorf("help: %d %q", code, out)
	}
	if code, _, errOut := run(); code != ExitUsage || !strings.Contains(errOut, "Usage") {
		t.Errorf("no command: %d %q", code, errOut)
	}
	if code, _, errOut := run("probe"); code != ExitUsage || !strings.Contains(errOut, "unknown command") {
		t.Errorf("unknown command: %d %q", code, errOut)
	}
	if code, _, errOut := run("check", "--help"); code != ExitOK || !strings.Contains(errOut, "-probe-root") {
		t.Errorf("check --help: %d %q", code, errOut)
	}
}

func TestCheckRejectsBadFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--no-such-flag"},
		{"extra"},
		{"--timeout", "0s"},
		{"--timeout", "-1s"},
		{"--timeout", "2h"},
		{"--timeout", "2562047h47m16s"},
		{"--max-abandoned", "0"},
		{"--rpc-floor", "-1ms"},
		{"--contended-ratio", "0"},
		{"--contended-ratio", "1.5"},
		{"--contended-floor", "-1ms"},
		{"--lock-dir", ""},
		{"--sysfs-root", ""},
		{"--client-id", "../x"},
		{"--client-id", "a b"},
	} {
		if code, _, errOut := run(append([]string{"check"}, args...)...); code != ExitUsage || errOut == "" {
			t.Errorf("%v: status %d, stderr %q", args, code, errOut)
		}
	}
}

// tree is a fake Lustre client: parameter trees, a probe root and a lock
// directory under one temporary directory.
type tree struct {
	t   *testing.T
	dir string
}

func newTree(t *testing.T) *tree {
	t.Helper()
	tr := &tree{t: t, dir: t.TempDir()}
	tr.device("mdc", "testfs-MDT0000-mdc-"+inst, "mds_conn_uuid", "10.0.0.1@tcp")
	tr.device("osc", "testfs-OST0000-osc-"+inst, "ost_conn_uuid", "10.0.0.2@tcp")
	tr.device("osc", "testfs-OST0001-osc-"+inst, "ost_conn_uuid", "10.0.0.3@tcp")
	return tr
}

func (tr *tree) write(rel, content string) {
	tr.t.Helper()
	path := filepath.Join(tr.dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		tr.t.Fatal(err)
	}
}

func (tr *tree) device(typ, name, uuid, nid string) {
	tr.write(filepath.Join("sys", typ, name, "kbytesfree"), "123456\n")
	tr.write(filepath.Join("sys", typ, name, uuid), nid+"\n")
	tr.write(filepath.Join("proc", typ, name, "state"), "current_state: FULL\n")
}

func (tr *tree) args(extra ...string) []string {
	return append([]string{"check",
		"--sysfs-root", filepath.Join(tr.dir, "sys"),
		"--procfs-root", filepath.Join(tr.dir, "proc"),
		"--debugfs-root", filepath.Join(tr.dir, "debug"),
		"--lock-dir", filepath.Join(tr.dir, "run", "lock"),
		// Local files answer in microseconds: any preemption would make a
		// step "contended". That rule is tested where the clock is faked.
		"--contended-floor", "1h",
	}, extra...)
}

// directIO reports whether the temporary directory supports O_DIRECT, which
// check always uses for the data probe.
func (tr *tree) directIO() bool {
	path := filepath.Join(tr.dir, "direct-check")
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_DIRECT, 0o600)
	if err != nil {
		return false
	}
	_ = syscall.Close(fd)
	_ = os.Remove(path)
	return true
}

func decode(t *testing.T, out string) check.Report {
	t.Helper()
	var rep check.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("%v in %q", err, out)
	}
	return rep
}

func steps(tg check.Target) string {
	var s []string
	for _, st := range tg.Steps {
		s = append(s, string(st.Step)+"="+string(st.Result))
	}
	return strings.Join(s, " ")
}

func TestCheckRPCOnly(t *testing.T) {
	tr := newTree(t)
	code, out, errOut := run(tr.args()...)
	if code != ExitOK {
		t.Fatalf("status %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{"testfs-MDT0000", "testfs-OST0000", "testfs-OST0001", "10.0.0.2@tcp", "FULL", "3 targets of testfs, 0 with a step that got no answer"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}

	code, out, _ = run(tr.args("--json")...)
	if code != ExitOK {
		t.Fatalf("json status %d", code)
	}
	rep := decode(t, out)
	if len(rep.Targets) != 3 || rep.Targets[0].Target != "testfs-MDT0000" || steps(rep.Targets[1]) != "rpc=ok" {
		t.Errorf("report %+v", rep)
	}
	// The lock directory was created and holds no lock afterwards.
	if _, err := os.Stat(filepath.Join(tr.dir, "run", "lock", "testfs-OST0000.lock")); err != nil {
		t.Errorf("lock file: %v", err)
	}
}

func TestCheckDataProbe(t *testing.T) {
	tr := newTree(t)
	if !tr.directIO() {
		t.Skip("the temporary directory does not support O_DIRECT")
	}
	root := filepath.Join(tr.dir, "root")
	tr.write(filepath.Join("root", "v1", "ost", "0000", "node-a001"), "")
	// OST0001 has no file: unprepared, and never created by check.
	lfs := filepath.Join(tr.dir, "lfs")
	if err := os.WriteFile(lfs, []byte("#!/bin/sh\nprintf 'lmm_stripe_count: 1\\nlmm_pattern: raid0\\nlmm_stripe_offset: 0\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := run(tr.args("--json", "--probe-root", root, "--client-id", "node-a001", "--lfs-path", lfs)...)
	if code != ExitFailed {
		t.Fatalf("status %d\n%s\n%s", code, out, errOut)
	}
	rep := decode(t, out)
	if got, want := steps(rep.Targets[1]), "rpc=ok open=ok verify=ok write=ok read=ok close=ok"; got != want {
		t.Errorf("OST0000: %q, want %q", got, want)
	}
	if got, want := steps(rep.Targets[2]), "rpc=ok open=unprepared verify=skipped write=skipped read=skipped close=skipped"; got != want {
		t.Errorf("OST0001: %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(root, "v1", "ost", "0001")); !os.IsNotExist(err) {
		t.Errorf("check created something for OST0001: %v", err)
	}
	for _, s := range rep.Targets[1].Steps {
		timed := s.Step == probe.StepRPC || s.Step == probe.StepOpen || s.Step == probe.StepWrite || s.Step == probe.StepRead
		if timed != (s.Nanos != nil) {
			t.Errorf("%s: nanos %v", s.Step, s.Nanos)
		}
	}

	// Without lfs the OST of the file is not verified, and nothing is written.
	code, out, _ = run(tr.args("--json", "--probe-root", root, "--client-id", "node-a001", "--lfs-path", filepath.Join(tr.dir, "none"))...)
	if got := steps(decode(t, out).Targets[1]); code != ExitFailed || !strings.Contains(got, "verify=unprepared write=skipped") {
		t.Errorf("without lfs: status %d, %q", code, got)
	}
	// Unless the operator says so.
	code, out, _ = run(tr.args("--json", "--probe-root", root, "--client-id", "node-a001", "--no-verify-pin")...)
	if got, want := steps(decode(t, out).Targets[1]), "rpc=ok open=ok verify=ok write=ok read=ok close=ok"; got != want {
		t.Errorf("--no-verify-pin: status %d, %q", code, got)
	}
}

// block replaces a device's kbytesfree by a FIFO, on which the rpc probe
// blocks until the test ends.
func (tr *tree) block(typ, name string) {
	tr.t.Helper()
	fifo := filepath.Join(tr.dir, "sys", typ, name, "kbytesfree")
	if err := os.Remove(fifo); err != nil {
		tr.t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		tr.t.Fatal(err)
	}
	tr.t.Cleanup(func() {
		// Opening the other end lets a blocked worker go.
		if f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = f.Close()
		}
	})
}

// workers lists the worker processes that hold a lock file of this tree:
// those in a visit, which after check has returned means those left blocked.
// (Counting processes would not do: with the race detector an exiting
// process lingers for a second.)
func (tr *tree) workers() []int {
	tr.t.Helper()
	lockDir := filepath.Join(tr.dir, "run", "lock") + string(filepath.Separator)
	var pids []int
	for _, pid := range liveWorkers() {
		fds, _ := os.ReadDir(filepath.Join("/proc", strconv.Itoa(pid), "fd"))
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "fd", fd.Name()))
			if err == nil && strings.HasPrefix(link, lockDir) {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids
}

func liveWorkers() []int {
	var pids []int
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		// "pid (comm) state ppid ..."; comm may contain spaces.
		rest := string(stat[bytes.LastIndexByte(stat, ')')+1:])
		fields := strings.Fields(rest)
		if len(fields) < 2 || fields[0] == "Z" || fields[1] != strconv.Itoa(os.Getpid()) {
			continue
		}
		cmdline, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if strings.HasSuffix(strings.TrimRight(string(cmdline), "\x00"), "\x00"+workerCommand) {
			pids = append(pids, pid)
		}
	}
	return pids
}

func TestCheckSurvivesABlockedTarget(t *testing.T) {
	tr := newTree(t)
	tr.block("osc", "testfs-OST0000-osc-"+inst)

	code, out, errOut := run(tr.args("--json", "--timeout", "200ms")...)
	if code != ExitFailed {
		t.Fatalf("status %d\n%s\n%s", code, out, errOut)
	}
	rep := decode(t, out)
	if steps(rep.Targets[1]) != "rpc=timeout" || steps(rep.Targets[2]) != "rpc=ok" || rep.Blocked != 1 {
		t.Errorf("report %+v", rep)
	}
	if len(rep.StuckTargets) != 1 || rep.StuckTargets[0] != "testfs-OST0000" {
		t.Errorf("stuck targets: %v", rep.StuckTargets)
	}

	// The worker left behind is in a session of its own, away from the
	// signals of the terminal that started check.
	left := tr.workers()
	if len(left) != 1 {
		t.Fatalf("%d workers left blocked, want 1", len(left))
	}
	pid := left[0]
	if sid, err := unix.Getsid(pid); err != nil || sid != pid {
		t.Errorf("worker %d: session %d, %v", pid, sid, err)
	}

	// A second run finds the first run's probe still blocked, and says so
	// although the worker is not its own.
	code, out, _ = run(tr.args("--json", "--timeout", "200ms")...)
	rep = decode(t, out)
	if code != ExitFailed || steps(rep.Targets[1]) != "rpc=stuck" || rep.Blocked != 0 {
		t.Errorf("second run: status %d, %+v", code, rep)
	}
	if len(rep.StuckTargets) != 1 || rep.StuckTargets[0] != "testfs-OST0000" {
		t.Errorf("second run: stuck targets %v", rep.StuckTargets)
	}
	code, out, _ = run(tr.args("--timeout", "200ms")...)
	if code != ExitFailed || !strings.Contains(out, "1 target(s) with a probe still blocked, from this run or an earlier one: testfs-OST0000") {
		t.Errorf("text report:\n%s", out)
	}
}

func TestRepeatedRunsBlockOneWorkerPerServerNotPerTarget(t *testing.T) {
	// Two OSTs of one server, both hanging, and check run again and again
	// as from cron: the first run blocks one worker; no later run may add
	// another by probing the second OST.
	tr := newTree(t)
	tr.device("osc", "testfs-OST0002-osc-"+inst, "ost_conn_uuid", "10.0.0.4@tcp")
	tr.device("osc", "testfs-OST0003-osc-"+inst, "ost_conn_uuid", "10.0.0.4@tcp")
	tr.block("osc", "testfs-OST0002-osc-"+inst)
	tr.block("osc", "testfs-OST0003-osc-"+inst)

	want := []string{"rpc=timeout", "rpc=stuck", "rpc=stuck"}
	for i, first := range want {
		code, out, errOut := run(tr.args("--json", "--timeout", "200ms")...)
		if code != ExitFailed {
			t.Fatalf("run %d: status %d\n%s\n%s", i, code, out, errOut)
		}
		rep := decode(t, out)
		if got := steps(rep.Targets[3]); got != first {
			t.Errorf("run %d: OST0002 %q, want %q", i, got, first)
		}
		if got := steps(rep.Targets[4]); got != "rpc=skipped" {
			t.Errorf("run %d: OST0003 %q, want it held", i, got)
		}
		if got := len(tr.workers()); got != 1 {
			t.Fatalf("run %d: %d workers blocked, want 1", i, got)
		}
	}
}

func TestConcurrentRunsDoNotCallEachOtherStuck(t *testing.T) {
	tr := newTree(t)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				if code, out, errOut := run(tr.args()...); code != ExitOK {
					t.Errorf("status %d\n%s\n%s", code, out, errOut)
				}
			}
		}()
	}
	wg.Wait()
}

func TestInheritedFilesAreClosedOnExec(t *testing.T) {
	// A file the invoker passed, such as the lock of "flock cmd".
	var p [2]int
	if err := syscall.Pipe(p[:]); err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(p[0])
	defer syscall.Close(p[1])
	flags, err := unix.FcntlInt(uintptr(p[1]), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC != 0 {
		t.Fatalf("before: flags %#x, %v", flags, err)
	}
	closeInheritedOnExec()
	for _, fd := range p {
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if err != nil || flags&unix.FD_CLOEXEC == 0 {
			t.Errorf("fd %d: flags %#x, %v", fd, flags, err)
		}
	}
}

func TestCheckErrors(t *testing.T) {
	tr := newTree(t)
	if code, _, errOut := run("check", "--sysfs-root", filepath.Join(tr.dir, "nowhere"), "--lock-dir", filepath.Join(tr.dir, "lock")); code != ExitFailed || !strings.Contains(errOut, "Lustre") {
		t.Errorf("no Lustre: %d %q", code, errOut)
	}
	if code, _, errOut := run(tr.args("--fsname", "other")...); code != ExitFailed || !strings.Contains(errOut, "other") {
		t.Errorf("unknown fsname: %d %q", code, errOut)
	}
	// A lock directory that cannot be created, or that others can write to:
	// an error of the run, before any target is probed.
	tr.write("file", "")
	if code, _, errOut := run("check", "--sysfs-root", filepath.Join(tr.dir, "sys"), "--lock-dir", filepath.Join(tr.dir, "file", "lock")); code != ExitFailed || !strings.Contains(errOut, "--lock-dir") {
		t.Errorf("bad lock dir: %d %q", code, errOut)
	}
	shared := filepath.Join(tr.dir, "shared")
	if err := os.Mkdir(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o1777); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := run("check", "--sysfs-root", filepath.Join(tr.dir, "sys"), "--lock-dir", shared); code != ExitFailed || !strings.Contains(errOut, "writable by other users") || out != "" {
		t.Errorf("shared lock dir: %d %q %q", code, out, errOut)
	}
}
