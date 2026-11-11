// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/yuuki/lustremesh/internal/probe"
	"github.com/yuuki/lustremesh/internal/target"
)

// maxRequestBytes bounds one request line.
const maxRequestBytes = 1 << 20

// Main runs a worker until its input ends or one of its steps times out.
// It returns the process exit status.
func Main(in io.Reader, out io.Writer) int {
	// The probes read the run-queue wait of their own thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	return mainLoop(&worker{out: json.NewEncoder(out)}, in)
}

func mainLoop(w *worker, in io.Reader) int {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64<<10), maxRequestBytes)
	for sc.Scan() {
		var req Request
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			w.emit(Event{Type: EventDone, Error: "malformed request: " + err.Error()})
			return 1
		}
		if !w.handle(req) {
			// A step timed out: this process has been given up on.
			return 0
		}
		if w.broken() {
			return 1
		}
	}
	return 0
}

type worker struct {
	mu      sync.Mutex // serializes out and guards failed
	out     *json.Encoder
	failed  bool
	backend backend // nil outside the tests
}

func (w *worker) emit(ev Event) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failed {
		return
	}
	if err := w.out.Encode(ev); err != nil {
		// The parent is gone; nobody is left to report to.
		w.failed = true
	}
}

func (w *worker) broken() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.failed
}

// handle carries out one request. It returns false when a step timed out.
func (w *worker) handle(req Request) (alive bool) {
	timeout := time.Duration(req.StepTimeoutNanos)
	if timeout <= 0 {
		w.emit(Event{ID: req.ID, Type: EventDone, Error: "request without a step timeout"})
		return true
	}
	r := &runner{w: w, id: req.ID, timeout: timeout}
	b := w.backend
	if b == nil {
		b = newRealBackend(req.Probe)
		defer b.close()
	}
	switch req.Op {
	case OpDiscover:
		return r.discover(b)
	case OpVisit:
		if req.Visit == nil {
			w.emit(Event{ID: req.ID, Type: EventDone, Error: "visit request without a visit"})
			return true
		}
		return r.visit(b, *req.Visit)
	default:
		w.emit(Event{ID: req.ID, Type: EventDone, Error: fmt.Sprintf("unknown operation %q", req.Op)})
		return true
	}
}

// runner runs the steps of one request under the watchdog.
type runner struct {
	w       *worker
	id      uint64
	timeout time.Duration
}

// step runs fn, which may never return. Exactly one of two things is
// reported for it: its result, if it returns before the deadline, or a
// timeout at the deadline followed, whenever fn returns, by its real
// duration. The mutex makes the choice atomic, so the parent never sees both.
func (r *runner) step(step probe.Step, fn func() probe.StepResult) (res probe.StepResult, inTime bool) {
	const (
		running = iota
		finished
		timedOut
	)
	var (
		mu    sync.Mutex
		state = running
	)
	r.w.emit(Event{ID: r.id, Type: EventBegin, Step: step})
	start := time.Now()
	timer := time.AfterFunc(r.timeout, func() {
		mu.Lock()
		defer mu.Unlock()
		if state == running {
			state = timedOut
			r.w.emit(Event{ID: r.id, Type: EventTimeout, Step: step})
		}
	})
	res = fn()
	mu.Lock()
	defer mu.Unlock()
	timer.Stop()
	if state == timedOut {
		r.w.emit(Event{ID: r.id, Type: EventLate, Step: step, LateNanos: int64(time.Since(start))})
		return res, false
	}
	state = finished
	res.Step = step
	r.w.emit(Event{ID: r.id, Type: EventStep, Step: step, Result: &res})
	return res, true
}

func (r *runner) skip(step probe.Step, why string) {
	res := probe.Skipped(step, why)
	r.w.emit(Event{ID: r.id, Type: EventStep, Step: step, Result: &res})
}

func (r *runner) discover(b backend) bool {
	var (
		devs    []target.Device
		skipped []string
		derr    error
	)
	res, inTime := r.step(probe.StepDiscover, func() probe.StepResult {
		devs, skipped, derr = b.discover()
		if derr != nil {
			return probe.StepResult{Result: probe.ResultError, Detail: derr.Error()}
		}
		return probe.StepResult{Result: probe.ResultOK}
	})
	if !inTime {
		return false
	}
	ev := Event{ID: r.id, Type: EventDone}
	if res.Result == probe.ResultOK {
		ev.Devices, ev.Skipped = devs, skipped
	} else {
		ev.Error = res.Detail
	}
	r.w.emit(ev)
	return true
}

func (r *runner) visit(b backend, v Visit) bool {
	// The name is checked again here: it becomes part of several paths.
	dev, err := target.ParseDeviceName(v.Device.Name)
	if err != nil {
		r.w.emit(Event{ID: r.id, Type: EventDone, Error: "invalid device: " + err.Error()})
		return true
	}
	dev.ServerNID = v.Device.ServerNID

	lock, held, err := acquire(v.LockDir, dev.Label())
	if err != nil {
		r.w.emit(Event{ID: r.id, Type: EventDone, Error: err.Error()})
		return true
	}
	if held {
		r.w.emit(Event{ID: r.id, Type: EventDone, Stuck: true})
		return true
	}
	// Held while the steps run: a step that never returns keeps the lock for
	// as long as it blocks, which is the point. Released before "done", so
	// that the parent may visit the target again at once.
	alive := r.steps(b, dev, v.Data)
	lock.release()
	if !alive {
		return false
	}
	r.w.emit(Event{ID: r.id, Type: EventDone})
	return true
}

// steps runs the steps of a visit. It returns false when one timed out.
func (r *runner) steps(b backend, dev target.Device, data *Data) bool {
	rpc, inTime := r.step(probe.StepRPC, func() probe.StepResult {
		// Sent before the probe, so that the state is known even when the
		// probe never returns.
		r.w.emit(Event{ID: r.id, Type: EventState, State: b.importState(dev)})
		return b.rpc(dev)
	})
	if !inTime {
		return false
	}
	if data != nil && dev.Kind == target.KindOST {
		return r.data(b, dev, *data, rpc)
	}
	return true
}

// data runs the data probe behind the rpc gate. It returns false when a
// step timed out.
func (r *runner) data(b backend, dev target.Device, d Data, rpc probe.StepResult) bool {
	steps := []probe.Step{probe.StepOpen}
	if d.VerifyPin {
		steps = append(steps, probe.StepPin)
	}
	steps = append(steps, probe.StepWrite, probe.StepRead, probe.StepClose)
	skipFrom := func(i int, why string) {
		for _, s := range steps[i:] {
			r.skip(s, why)
		}
	}

	// The gate: no I/O is sent to a target whose import did not just
	// answer, because that I/O would block and could not be interrupted.
	if !rpc.Result.Answered() {
		skipFrom(0, fmt.Sprintf("the rpc probe was %s", rpc.Result))
		return true
	}

	var f dataFile
	open, inTime := r.step(probe.StepOpen, func() probe.StepResult {
		var res probe.StepResult
		f, res = b.openData(probe.DataSpec{Root: d.Root, IndexHex: dev.IndexHex, ClientID: d.ClientID, Direct: d.Direct})
		return res
	})
	if !inTime {
		return false
	}
	if !open.Result.Answered() || f == nil {
		skipFrom(1, fmt.Sprintf("open was %s", open.Result))
		return true
	}

	// From here on the file is open and is closed under the watchdog too.
	i := 1
	why := ""
	if d.VerifyPin {
		pin, inTime := r.step(probe.StepPin, func() probe.StepResult { return f.Pin(d.LfsPath, dev.Index) })
		if !inTime {
			return false
		}
		i++
		if pin.Result != probe.ResultOK {
			why = "the file is not verified to be on " + dev.Label()
		}
	}
	if why == "" {
		wr, inTime := r.step(probe.StepWrite, func() probe.StepResult { return f.Write() })
		if !inTime {
			return false
		}
		i++
		if !wr.Result.Answered() {
			why = fmt.Sprintf("write was %s", wr.Result)
		}
	}
	if why == "" {
		if _, inTime := r.step(probe.StepRead, func() probe.StepResult { return f.Read() }); !inTime {
			return false
		}
		i++
	}
	// Skip what was not attempted, up to but not including close.
	for _, s := range steps[i : len(steps)-1] {
		r.skip(s, why)
	}
	_, inTime = r.step(probe.StepClose, func() probe.StepResult { return f.Close() })
	return inTime
}

// backend is what a worker does to Lustre; the tests replace it.
type backend interface {
	discover() ([]target.Device, []string, error)
	importState(target.Device) string
	rpc(target.Device) probe.StepResult
	openData(probe.DataSpec) (dataFile, probe.StepResult)
	close()
}

type dataFile interface {
	Pin(lfsPath string, want uint32) probe.StepResult
	Write() probe.StepResult
	Read() probe.StepResult
	Close() probe.StepResult
}

type realBackend struct {
	cfg probe.Config
	p   *probe.Prober
}

func newRealBackend(cfg probe.Config) *realBackend {
	return &realBackend{cfg: cfg, p: probe.NewProber(cfg)}
}

func (b *realBackend) discover() ([]target.Device, []string, error) { return probe.Discover(b.cfg) }
func (b *realBackend) importState(d target.Device) string           { return b.p.ImportState(d) }
func (b *realBackend) rpc(d target.Device) probe.StepResult         { return b.p.RPC(d) }
func (b *realBackend) close()                                       { b.p.Close() }

func (b *realBackend) openData(s probe.DataSpec) (dataFile, probe.StepResult) {
	f, res := b.p.OpenData(s)
	if f == nil {
		// A nil *probe.DataFile must not become a non-nil interface.
		return nil, res
	}
	return f, res
}

// lock is a held target lock.
type lock struct{ f *os.File }

// acquire takes the lock of one target without waiting. held is true when
// another process has it.
func acquire(dir, label string) (l *lock, held bool, err error) {
	if dir == "" {
		return nil, false, errors.New("no lock directory")
	}
	path := filepath.Join(dir, label+".lock")
	// Read-only is enough for flock and lets users share the lock files.
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o644) //nolint:gosec // label is a validated target name
	if err != nil {
		return nil, false, fmt.Errorf("lock file: %w", err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		_ = f.Close()
		return nil, true, nil
	}
	if err != nil {
		_ = f.Close()
		return nil, false, fmt.Errorf("lock %s: %w", path, err)
	}
	return &lock{f: f}, false, nil
}

func (l *lock) release() {
	if l != nil && l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
}
