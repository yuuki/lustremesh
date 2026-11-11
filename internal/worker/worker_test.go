// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yuuki/lustremesh/internal/probe"
	"github.com/yuuki/lustremesh/internal/target"
)

const inst = "ffff8f0b3c5e2000"

func ok(step probe.Step) probe.StepResult {
	return probe.StepResult{Step: step, Result: probe.ResultOK}
}

// fakeBackend scripts what Lustre does. block, when not nil, is waited for
// inside the named step: the step hangs until the test closes it.
type fakeBackend struct {
	rpcResult probe.StepResult
	openRes   probe.StepResult
	file      *fakeFile
	blockStep probe.Step
	block     chan struct{}

	mu    sync.Mutex
	calls []string
}

func (b *fakeBackend) called(name string) {
	b.mu.Lock()
	b.calls = append(b.calls, name)
	b.mu.Unlock()
}

func (b *fakeBackend) wait(step probe.Step) {
	if b.block != nil && b.blockStep == step {
		<-b.block
	}
}

func (b *fakeBackend) discover() ([]target.Device, []string, error) {
	b.called("discover")
	b.wait(probe.StepDiscover)
	d, _ := target.ParseDeviceName("testfs-OST0000-osc-" + inst)
	return []target.Device{d}, []string{"osc/odd"}, nil
}
func (b *fakeBackend) importState(target.Device) string { return "FULL" }
func (b *fakeBackend) rpc(target.Device) probe.StepResult {
	b.called("rpc")
	b.wait(probe.StepRPC)
	return b.rpcResult
}
func (b *fakeBackend) openData(probe.DataSpec) (dataFile, probe.StepResult) {
	b.called("open")
	b.wait(probe.StepOpen)
	if b.file == nil {
		return nil, b.openRes
	}
	b.file.b = b
	return b.file, b.openRes
}
func (b *fakeBackend) close() {}

type fakeFile struct {
	b                    *fakeBackend
	pin, write, read, cl probe.StepResult
}

func (f *fakeFile) Pin(string, uint32) probe.StepResult {
	f.b.called("pin")
	f.b.wait(probe.StepPin)
	return f.pin
}
func (f *fakeFile) Write() probe.StepResult {
	f.b.called("write")
	f.b.wait(probe.StepWrite)
	return f.write
}
func (f *fakeFile) Read() probe.StepResult {
	f.b.called("read")
	f.b.wait(probe.StepRead)
	return f.read
}
func (f *fakeFile) Close() probe.StepResult {
	f.b.called("close")
	f.b.wait(probe.StepClose)
	return f.cl
}

func healthyFile() *fakeFile {
	return &fakeFile{pin: ok(probe.StepPin), write: ok(probe.StepWrite), read: ok(probe.StepRead), cl: ok(probe.StepClose)}
}

// harness runs a worker in a goroutine and talks to it through pipes.
type harness struct {
	t      *testing.T
	in     *io.PipeWriter
	events chan Event
	exit   chan int
}

func startWorker(t *testing.T, b backend) *harness {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h := &harness{t: t, in: inW, events: make(chan Event, 64), exit: make(chan int, 1)}
	go func() {
		w := &worker{out: json.NewEncoder(outW), backend: b}
		h.exit <- mainLoop(w, inR)
		_ = outW.Close()
	}()
	go func() {
		defer close(h.events)
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			var ev Event
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
				t.Errorf("bad event %q: %v", sc.Text(), err)
				return
			}
			h.events <- ev
		}
	}()
	t.Cleanup(func() { _ = inW.Close() })
	return h
}

func (h *harness) send(req Request) {
	h.t.Helper()
	line, err := json.Marshal(req)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.in.Write(append(line, '\n')); err != nil {
		h.t.Fatal(err)
	}
}

// until collects events up to and including one of the given type.
func (h *harness) until(typ string) []Event {
	h.t.Helper()
	var got []Event
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, open := <-h.events:
			if !open {
				h.t.Fatalf("worker output ended before %q; got %+v", typ, got)
			}
			got = append(got, ev)
			if ev.Type == typ {
				return got
			}
		case <-deadline:
			h.t.Fatalf("no %q event; got %+v", typ, got)
		}
	}
}

// summary renders the step outcomes of a request as "rpc=ok open=skipped".
func summary(events []Event) string {
	var parts []string
	for _, ev := range events {
		switch ev.Type {
		case EventStep:
			parts = append(parts, string(ev.Step)+"="+string(ev.Result.Result))
		case EventTimeout:
			parts = append(parts, string(ev.Step)+"=TIMEOUT")
		case EventLate:
			parts = append(parts, string(ev.Step)+"=LATE")
		}
	}
	return strings.Join(parts, " ")
}

func visitRequest(t *testing.T, id uint64, data *Data, timeout time.Duration) Request {
	t.Helper()
	dev, err := target.ParseDeviceName("testfs-OST0003-osc-" + inst)
	if err != nil {
		t.Fatal(err)
	}
	return Request{ID: id, Op: OpVisit, StepTimeoutNanos: int64(timeout), Visit: &Visit{Device: dev, LockDir: t.TempDir(), Data: data}}
}

func TestVisitRunsEveryStepInOrder(t *testing.T) {
	b := &fakeBackend{rpcResult: ok(probe.StepRPC), openRes: ok(probe.StepOpen), file: healthyFile()}
	h := startWorker(t, b)
	h.send(visitRequest(t, 1, &Data{Root: "/probe", ClientID: "node-a001", VerifyPin: true}, time.Minute))
	events := h.until(EventDone)
	if got, want := summary(events), "rpc=ok open=ok pin=ok write=ok read=ok close=ok"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	var state string
	for _, ev := range events {
		if ev.Type == EventState {
			state = ev.State
		}
		if ev.ID != 1 {
			t.Errorf("event for request %d", ev.ID)
		}
	}
	if state != "FULL" {
		t.Errorf("state %q", state)
	}
}

func TestVisitWithoutData(t *testing.T) {
	b := &fakeBackend{rpcResult: ok(probe.StepRPC)}
	h := startWorker(t, b)
	h.send(visitRequest(t, 1, nil, time.Minute))
	if got := summary(h.until(EventDone)); got != "rpc=ok" {
		t.Errorf("got %q", got)
	}
}

func TestGateKeepsDataFromUnansweredTarget(t *testing.T) {
	for _, res := range []probe.Result{probe.ResultDisconnected, probe.ResultInactive, probe.ResultError} {
		b := &fakeBackend{rpcResult: probe.StepResult{Result: res}, openRes: ok(probe.StepOpen), file: healthyFile()}
		h := startWorker(t, b)
		h.send(visitRequest(t, 1, &Data{VerifyPin: true}, time.Minute))
		want := "rpc=" + string(res) + " open=skipped pin=skipped write=skipped read=skipped close=skipped"
		if got := summary(h.until(EventDone)); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		for _, c := range b.calls {
			if c != "rpc" {
				t.Errorf("rpc %s: %q was called", res, c)
			}
		}
	}
}

func TestCachedAndContendedPassTheGate(t *testing.T) {
	for _, res := range []probe.Result{probe.ResultCached, probe.ResultContended} {
		b := &fakeBackend{rpcResult: probe.StepResult{Result: res}, openRes: ok(probe.StepOpen), file: healthyFile()}
		h := startWorker(t, b)
		h.send(visitRequest(t, 1, &Data{}, time.Minute))
		want := "rpc=" + string(res) + " open=ok write=ok read=ok close=ok"
		if got := summary(h.until(EventDone)); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestUnverifiedPinIsNeverWritten(t *testing.T) {
	f := healthyFile()
	f.pin = probe.StepResult{Result: probe.ResultUnprepared, Detail: "on OST 5"}
	b := &fakeBackend{rpcResult: ok(probe.StepRPC), openRes: ok(probe.StepOpen), file: f}
	h := startWorker(t, b)
	h.send(visitRequest(t, 1, &Data{VerifyPin: true}, time.Minute))
	if got, want := summary(h.until(EventDone)), "rpc=ok open=ok pin=unprepared write=skipped read=skipped close=ok"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	for _, c := range b.calls {
		if c == "write" || c == "read" {
			t.Errorf("%s was called on an unverified file", c)
		}
	}
}

func TestFailedOpenAndFailedWrite(t *testing.T) {
	b := &fakeBackend{rpcResult: ok(probe.StepRPC), openRes: probe.StepResult{Result: probe.ResultUnprepared}}
	h := startWorker(t, b)
	h.send(visitRequest(t, 1, &Data{}, time.Minute))
	if got, want := summary(h.until(EventDone)), "rpc=ok open=unprepared write=skipped read=skipped close=skipped"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	f := healthyFile()
	f.write = probe.StepResult{Result: probe.ResultError, Errno: "EIO"}
	b = &fakeBackend{rpcResult: ok(probe.StepRPC), openRes: ok(probe.StepOpen), file: f}
	h = startWorker(t, b)
	h.send(visitRequest(t, 1, &Data{}, time.Minute))
	if got, want := summary(h.until(EventDone)), "rpc=ok open=ok write=error read=skipped close=ok"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBlockedStepTimesOutThenReportsLateAndExits(t *testing.T) {
	for _, step := range []probe.Step{probe.StepRPC, probe.StepOpen, probe.StepPin, probe.StepWrite, probe.StepRead, probe.StepClose} {
		t.Run(string(step), func(t *testing.T) {
			b := &fakeBackend{rpcResult: ok(probe.StepRPC), openRes: ok(probe.StepOpen), file: healthyFile(),
				blockStep: step, block: make(chan struct{})}
			h := startWorker(t, b)
			req := visitRequest(t, 7, &Data{VerifyPin: true}, 50*time.Millisecond)
			h.send(req)
			events := h.until(EventTimeout)
			last := events[len(events)-1]
			if last.Step != step || last.ID != 7 {
				t.Fatalf("timeout event %+v", last)
			}
			for _, ev := range events {
				if ev.Type == EventStep && ev.Step == step {
					t.Fatalf("a result was reported for the blocked step: %+v", ev)
				}
			}

			// While it is blocked the worker says nothing more and keeps the lock.
			select {
			case ev := <-h.events:
				t.Fatalf("event while blocked: %+v", ev)
			case <-time.After(100 * time.Millisecond):
			}
			if _, held, err := acquire(req.Visit.LockDir, "testfs-OST0003"); err != nil || !held {
				t.Fatalf("lock while blocked: held=%v err=%v", held, err)
			}

			close(b.block)
			events = h.until(EventLate)
			late := events[len(events)-1]
			if late.Step != step || late.LateNanos < int64(150*time.Millisecond) {
				t.Fatalf("late event %+v", late)
			}
			select {
			case code := <-h.exit:
				if code != 0 {
					t.Errorf("exit status %d", code)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the worker did not exit after its late step")
			}
			if ev, open := <-h.events; open {
				t.Errorf("event after late: %+v", ev)
			}
			// Nothing ran after the step that timed out.
			if last := b.calls[len(b.calls)-1]; last != string(step) {
				t.Errorf("%q ran after the blocked %s", last, step)
			}
		})
	}
}

func TestStepFinishingAtTheDeadlineReportsExactlyOneOutcome(t *testing.T) {
	// The step returns right around its deadline, many times: whichever
	// side wins, the parent must see a result or a timeout, never both.
	for i := 0; i < 200; i++ {
		var mu sync.Mutex
		var out []Event
		w := &worker{out: json.NewEncoder(writerFunc(func(p []byte) (int, error) {
			var ev Event
			if err := json.Unmarshal(p, &ev); err != nil {
				return 0, err
			}
			mu.Lock()
			out = append(out, ev)
			mu.Unlock()
			return len(p), nil
		}))}
		r := &runner{w: w, id: 1, timeout: time.Millisecond}
		r.step(probe.StepWrite, func() probe.StepResult {
			time.Sleep(time.Millisecond)
			return ok(probe.StepWrite)
		})
		time.Sleep(2 * time.Millisecond) // let a pending timer run
		mu.Lock()
		got := summary(out)
		mu.Unlock()
		if got != "write=ok" && got != "write=TIMEOUT write=LATE" {
			t.Fatalf("iteration %d: %q", i, got)
		}
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestHeldLockMeansStuck(t *testing.T) {
	b := &fakeBackend{rpcResult: ok(probe.StepRPC)}
	h := startWorker(t, b)
	req := visitRequest(t, 1, nil, time.Minute)
	l, held, err := acquire(req.Visit.LockDir, "testfs-OST0003")
	if err != nil || held {
		t.Fatalf("acquire: held=%v err=%v", held, err)
	}
	h.send(req)
	events := h.until(EventDone)
	if !events[len(events)-1].Stuck || len(b.calls) != 0 {
		t.Fatalf("events %+v, calls %v", events, b.calls)
	}

	// Released, the same worker probes the target.
	l.release()
	req.ID = 2
	h.send(req)
	if got := summary(h.until(EventDone)); got != "rpc=ok" {
		t.Errorf("after release: %q", got)
	}
	// And released its own lock at the end of the visit.
	if l, held, err := acquire(req.Visit.LockDir, "testfs-OST0003"); err != nil || held {
		t.Errorf("lock after the visit: held=%v err=%v", held, err)
	} else {
		l.release()
	}
}

func TestLockSurvivesOnlyAsLongAsItsHolder(t *testing.T) {
	// The lock is a flock on an open file: a process that dies, however it
	// dies, releases it. Closing the descriptor stands for that here.
	dir := t.TempDir()
	path := filepath.Join(dir, "testfs-OST0000.lock")
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if _, held, _ := acquire(dir, "testfs-OST0000"); !held {
		t.Fatal("not held")
	}
	_ = f.Close()
	l, held, err := acquire(dir, "testfs-OST0000")
	if err != nil || held {
		t.Fatalf("after close: held=%v err=%v", held, err)
	}
	l.release()

	if _, _, err := acquire(filepath.Join(dir, "missing"), "x"); err == nil {
		t.Error("missing lock directory: no error")
	}
	if _, _, err := acquire("", "x"); err == nil {
		t.Error("empty lock directory: no error")
	}
}

func TestDiscover(t *testing.T) {
	b := &fakeBackend{}
	h := startWorker(t, b)
	h.send(Request{ID: 3, Op: OpDiscover, StepTimeoutNanos: int64(time.Minute)})
	events := h.until(EventDone)
	done := events[len(events)-1]
	if len(done.Devices) != 1 || done.Devices[0].Label() != "testfs-OST0000" || len(done.Skipped) != 1 || done.Error != "" {
		t.Errorf("done %+v", done)
	}
}

func TestBadRequests(t *testing.T) {
	dev, _ := target.ParseDeviceName("testfs-OST0003-osc-" + inst)
	bad := dev
	bad.Name = "../../etc/passwd"
	tests := []struct {
		name string
		req  Request
	}{
		{"no timeout", Request{ID: 1, Op: OpVisit, Visit: &Visit{Device: dev, LockDir: t.TempDir()}}},
		{"unknown operation", Request{ID: 1, Op: "format", StepTimeoutNanos: 1e9}},
		{"visit without a visit", Request{ID: 1, Op: OpVisit, StepTimeoutNanos: 1e9}},
		{"device name that is a path", Request{ID: 1, Op: OpVisit, StepTimeoutNanos: 1e9, Visit: &Visit{Device: bad, LockDir: t.TempDir()}}},
		{"no lock directory", Request{ID: 1, Op: OpVisit, StepTimeoutNanos: 1e9, Visit: &Visit{Device: dev}}},
	}
	for _, tc := range tests {
		b := &fakeBackend{rpcResult: ok(probe.StepRPC)}
		h := startWorker(t, b)
		h.send(tc.req)
		events := h.until(EventDone)
		if done := events[len(events)-1]; done.Error == "" || len(b.calls) != 0 {
			t.Errorf("%s: done %+v, calls %v", tc.name, done, b.calls)
		}
	}
}

func TestMalformedInputEndsTheWorker(t *testing.T) {
	h := startWorker(t, &fakeBackend{})
	if _, err := h.in.Write([]byte("{not json\n")); err != nil {
		t.Fatal(err)
	}
	events := h.until(EventDone)
	if events[len(events)-1].Error == "" {
		t.Errorf("events %+v", events)
	}
	if code := <-h.exit; code != 1 {
		t.Errorf("exit status %d", code)
	}
}

func TestWorkerExitsWhenItsInputCloses(t *testing.T) {
	h := startWorker(t, &fakeBackend{})
	_ = h.in.Close()
	select {
	case code := <-h.exit:
		if code != 0 {
			t.Errorf("exit status %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still running")
	}
}

func TestWorkerStopsWhenNobodyListens(t *testing.T) {
	w := &worker{out: json.NewEncoder(writerFunc(func([]byte) (int, error) { return 0, errors.New("broken pipe") })),
		backend: &fakeBackend{rpcResult: ok(probe.StepRPC)}}
	dev, _ := target.ParseDeviceName("testfs-OST0003-osc-" + inst)
	req, _ := json.Marshal(Request{ID: 1, Op: OpVisit, StepTimeoutNanos: 1e9, Visit: &Visit{Device: dev, LockDir: t.TempDir()}})
	if code := mainLoop(w, strings.NewReader(string(req)+"\n"+string(req)+"\n")); code != 1 {
		t.Errorf("exit status %d", code)
	}
}
