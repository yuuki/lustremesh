// SPDX-License-Identifier: Apache-2.0

//go:build linux

package supervisor

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yuuki/lustremesh/internal/probe"
	"github.com/yuuki/lustremesh/internal/target"
	"github.com/yuuki/lustremesh/internal/worker"
)

const (
	inst    = "ffff8f0b3c5e2000"
	modeEnv = "LUSTREMESH_TEST_WORKER"
)

// TestMain turns the test binary into a worker process when asked to: the
// tests start real child processes, as the supervisor does in production.
func TestMain(m *testing.M) {
	switch os.Getenv(modeEnv) {
	case "":
		os.Exit(m.Run())
	case "worker":
		os.Exit(worker.Main(os.Stdin, os.Stdout))
	case "silent": // reads its requests and never answers
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	case "dead": // exits at once
		os.Exit(3)
	}
}

type env struct {
	t    *testing.T
	dir  string
	sup  *Supervisor
	mode string
}

func newEnv(t *testing.T, timeout time.Duration) *env {
	t.Helper()
	e := &env{t: t, dir: t.TempDir(), mode: "worker"}
	lockDir := filepath.Join(e.dir, "lock")
	if err := os.Mkdir(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	e.sup = New(Config{
		Command: func() *exec.Cmd {
			cmd := exec.Command(os.Args[0])
			cmd.Env = append(os.Environ(), modeEnv+"="+e.mode)
			cmd.Stderr = os.Stderr
			return cmd
		},
		StepTimeout: timeout,
		Grace:       200 * time.Millisecond,
		LockDir:     lockDir,
		Probe: probe.Config{
			SysfsRoot:      filepath.Join(e.dir, "sys"),
			ProcfsRoot:     filepath.Join(e.dir, "proc"),
			DebugfsRoot:    filepath.Join(e.dir, "debug"),
			ContendedRatio: 1, ContendedFloorNanos: int64(time.Hour),
		},
	})
	t.Cleanup(e.sup.Close)
	return e
}

func (e *env) write(path, content string) {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) device(name, nid, state string) target.Device {
	e.t.Helper()
	d, err := target.ParseDeviceName(name)
	if err != nil {
		e.t.Fatal(err)
	}
	dir := filepath.Join(e.dir, "sys", d.ClientType(), name)
	e.write(filepath.Join(dir, "kbytesfree"), "123456\n")
	uuid := "ost_conn_uuid"
	if d.Kind == target.KindMDT {
		uuid = "mds_conn_uuid"
	}
	e.write(filepath.Join(dir, uuid), nid+"\n")
	e.write(filepath.Join(e.dir, "proc", d.ClientType(), name, "state"), "current_state: "+state+"\n")
	d.ServerNID = nid
	return d
}

// hang replaces the device's kbytesfree by a FIFO: the rpc probe blocks
// until release is called.
func (e *env) hang(d target.Device) (release func()) {
	e.t.Helper()
	path := filepath.Join(e.dir, "sys", d.ClientType(), d.Name, "kbytesfree")
	if err := os.Remove(path); err != nil {
		e.t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		e.t.Fatal(err)
	}
	released := false
	release = func() {
		if released {
			return
		}
		released = true
		// Opening the other end lets the worker's open return.
		f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return // no reader: no worker is blocked on it
		}
		_, _ = f.WriteString("42\n")
		_ = f.Close()
	}
	e.t.Cleanup(release)
	return release
}

func (e *env) dataFile(d target.Device, clientID string) *worker.Data {
	e.t.Helper()
	root := filepath.Join(e.dir, "root")
	dir := filepath.Join(root, "v1", "ost", d.IndexHex)
	e.write(filepath.Join(dir, clientID), "")
	direct := true
	fd, err := syscall.Open(filepath.Join(dir, "direct-check"), syscall.O_RDWR|syscall.O_CREAT|syscall.O_DIRECT, 0o600)
	if err != nil {
		direct = false
	} else {
		_ = syscall.Close(fd)
	}
	_ = os.Remove(filepath.Join(dir, "direct-check"))
	return &worker.Data{Root: root, ClientID: clientID, Direct: direct}
}

func summary(r VisitReport) string {
	var parts []string
	for _, s := range r.Steps {
		parts = append(parts, string(s.Step)+"="+string(s.Result))
	}
	return strings.Join(parts, " ")
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDiscoverAndVisit(t *testing.T) {
	e := newEnv(t, 5*time.Second)
	e.device("testfs-MDT0000-mdc-"+inst, "10.0.0.1@tcp", "FULL")
	ost := e.device("testfs-OST0003-osc-"+inst, "10.0.0.2@tcp", "IDLE")
	ctx := context.Background()

	devs, _, err := e.sup.Discover(ctx)
	if err != nil || len(devs) != 2 {
		t.Fatalf("discover: %+v, %v", devs, err)
	}

	r := e.sup.Visit(ctx, ost, nil)
	if summary(r) != "rpc=ok" || r.State != "IDLE" {
		t.Errorf("rpc only: %q state %q", summary(r), r.State)
	}

	data := e.dataFile(ost, "node-a001")
	r = e.sup.Visit(ctx, ost, data)
	if got, want := summary(r), "rpc=ok open=ok write=ok read=ok close=ok"; got != want {
		t.Errorf("data: %q, want %q (%+v)", got, want, r.Steps)
	}

	// With pin verification and no usable lfs, nothing is written.
	data.VerifyPin = true
	data.LfsPath = filepath.Join(e.dir, "no-such-lfs")
	before, _ := os.ReadFile(filepath.Join(data.Root, "v1", "ost", ost.IndexHex, "node-a001"))
	r = e.sup.Visit(ctx, ost, data)
	if got, want := summary(r), "rpc=ok open=ok pin=unprepared write=skipped read=skipped close=ok"; got != want {
		t.Errorf("unverified pin: %q, want %q", got, want)
	}
	after, _ := os.ReadFile(filepath.Join(data.Root, "v1", "ost", ost.IndexHex, "node-a001"))
	if string(before) != string(after) {
		t.Error("the file was rewritten although its OST was not verified")
	}
	if e.sup.Abandoned() != 0 {
		t.Errorf("abandoned = %d", e.sup.Abandoned())
	}
}

func TestBlockedProbeIsAbandonedNotWaitedFor(t *testing.T) {
	e := newEnv(t, 200*time.Millisecond)
	bad := e.device("testfs-OST0001-osc-"+inst, "10.0.0.2@tcp", "FULL")
	good := e.device("testfs-OST0002-osc-"+inst, "10.0.0.3@tcp", "FULL")
	release := e.hang(bad)
	ctx := context.Background()

	start := time.Now()
	r := e.sup.Visit(ctx, bad, e.dataFile(bad, "node-a001"))
	if got, want := summary(r), "rpc=timeout open=skipped write=skipped read=skipped close=skipped"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if r.State != "FULL" {
		t.Errorf("state of a blocked probe: %q", r.State)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("the visit took %v", d)
	}
	if e.sup.Abandoned() != 1 {
		t.Fatalf("abandoned = %d", e.sup.Abandoned())
	}

	// A new worker carries on with the other targets.
	if got := summary(e.sup.Visit(ctx, good, nil)); got != "rpc=ok" {
		t.Errorf("other target: %q", got)
	}
	// The blocked target is not probed again while its worker is blocked.
	if got := summary(e.sup.Visit(ctx, bad, nil)); got != "rpc=stuck" {
		t.Errorf("blocked target again: %q", got)
	}
	if len(e.sup.Stalls()) != 0 {
		t.Errorf("stalls before the release: %v", e.sup.Stalls())
	}

	// The call returns at last: its real duration is reported, the worker
	// exits, and the target can be probed again.
	release()
	waitFor(t, "the abandoned worker to exit", func() bool { return e.sup.Abandoned() == 0 })
	stall, ok := e.sup.Stalls()["testfs-OST0001"]
	if !ok || stall.Step != probe.StepRPC || stall.Nanos < int64(200*time.Millisecond) {
		t.Errorf("stall: %+v, %v", stall, ok)
	}
	path := filepath.Join(e.dir, "sys", "osc", bad.Name, "kbytesfree")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	e.write(path, "1\n")
	if got := summary(e.sup.Visit(ctx, bad, nil)); got != "rpc=ok" {
		t.Errorf("after recovery: %q", got)
	}
}

func TestSilentWorkerIsGivenUpAfterTheGrace(t *testing.T) {
	e := newEnv(t, 100*time.Millisecond)
	e.mode = "silent"
	d := e.device("testfs-OST0001-osc-"+inst, "10.0.0.2@tcp", "FULL")
	start := time.Now()
	r := e.sup.Visit(context.Background(), d, e.dataFile(d, "node-a001"))
	if got, want := summary(r), "rpc=timeout open=skipped write=skipped read=skipped close=skipped"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if d := time.Since(start); d < 300*time.Millisecond || d > 3*time.Second {
		t.Errorf("gave up after %v", d)
	}
	// Its input is closed, so this worker ends by itself.
	waitFor(t, "the silent worker to exit", func() bool { return e.sup.Abandoned() == 0 })
}

func TestDeadWorkerIsAnErrorNotATimeout(t *testing.T) {
	e := newEnv(t, 5*time.Second)
	e.mode = "dead"
	d := e.device("testfs-OST0001-osc-"+inst, "10.0.0.2@tcp", "FULL")
	r := e.sup.Visit(context.Background(), d, nil)
	if summary(r) != "rpc=error" || r.Steps[0].Detail == "" {
		t.Errorf("got %+v", r.Steps)
	}
	if _, _, err := e.sup.Discover(context.Background()); err == nil {
		t.Error("discover with a dead worker: no error")
	}
	if e.sup.Abandoned() != 0 {
		t.Errorf("abandoned = %d", e.sup.Abandoned())
	}

	// The next request starts a fresh worker.
	e.mode = "worker"
	if got := summary(e.sup.Visit(context.Background(), d, nil)); got != "rpc=ok" {
		t.Errorf("after the dead worker: %q", got)
	}
}

func TestWorkerThatCannotStart(t *testing.T) {
	sup := New(Config{
		Command:     func() *exec.Cmd { return exec.Command(filepath.Join(t.TempDir(), "missing")) },
		StepTimeout: time.Second, Grace: time.Second,
	})
	d, _ := target.ParseDeviceName("testfs-OST0001-osc-" + inst)
	r := sup.Visit(context.Background(), d, nil)
	if summary(r) != "rpc=error" {
		t.Errorf("got %q", summary(r))
	}
}

func TestInterruptedVisit(t *testing.T) {
	e := newEnv(t, 30*time.Second)
	d := e.device("testfs-OST0001-osc-"+inst, "10.0.0.2@tcp", "FULL")
	release := e.hang(d)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	r := e.sup.Visit(ctx, d, nil)
	if summary(r) != "rpc=error" || !strings.Contains(r.Steps[0].Detail, "interrupted") {
		t.Errorf("got %+v", r.Steps)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("returned after %v", d)
	}
	release()
	waitFor(t, "the interrupted worker to exit", func() bool { return e.sup.Abandoned() == 0 })
}

func TestDiscoverTimeout(t *testing.T) {
	e := newEnv(t, 100*time.Millisecond)
	e.mode = "silent"
	if _, _, err := e.sup.Discover(context.Background()); err == nil || !strings.Contains(err.Error(), "did not return") {
		t.Errorf("err = %v", err)
	}
}

func TestSteps(t *testing.T) {
	ost, _ := target.ParseDeviceName("testfs-OST0001-osc-" + inst)
	mdt, _ := target.ParseDeviceName("testfs-MDT0000-mdc-" + inst)
	join := func(steps []probe.Step) string {
		var s []string
		for _, st := range steps {
			s = append(s, string(st))
		}
		return strings.Join(s, " ")
	}
	if got := join(Steps(ost, nil)); got != "rpc" {
		t.Errorf("ost, no data: %q", got)
	}
	if got := join(Steps(mdt, &worker.Data{VerifyPin: true})); got != "rpc" {
		t.Errorf("mdt: %q", got)
	}
	if got := join(Steps(ost, &worker.Data{})); got != "rpc open write read close" {
		t.Errorf("ost: %q", got)
	}
	if got := join(Steps(ost, &worker.Data{VerifyPin: true})); got != "rpc open pin write read close" {
		t.Errorf("ost with pin: %q", got)
	}
}
