// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
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
	// The input is read apart from the steps, so that its end is noticed
	// even while a step runs: a parent that closes it has given this worker
	// up, and the worker must not start anything more.
	lines := make(chan []byte)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(in)
		sc.Buffer(make([]byte, 0, 64<<10), maxRequestBytes)
		for sc.Scan() {
			lines <- append([]byte(nil), sc.Bytes()...)
		}
		w.orphan.Store(true)
	}()
	for line := range lines {
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			w.emit(Event{Type: EventDone, Error: "malformed request: " + err.Error()})
			return 1
		}
		if !w.handle(req) {
			// A step timed out, or the parent is gone: this process has
			// been given up on.
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
	orphan  atomic.Bool // the input ended: the parent expects nothing more
	backend backend     // nil outside the tests
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
	r := &runner{w: w, id: req.ID, timeout: timeout, silence: time.Duration(req.SilenceNanos)}
	if req.SentUnixNanos > 0 {
		r.progress = time.Unix(0, req.SentUnixNanos)
	}
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
	// onTimeout runs when a step passes its deadline, before the timeout is
	// reported.
	onTimeout func()
	// progress is when the parent last had news of this request: when it
	// sent it, then each time a step ended. Past silence without news the
	// parent gives the worker up. Both are zero when the parent set no limit.
	progress time.Time
	silence  time.Duration
}

// givenUp reports whether the parent has stopped listening: it closed the
// input, or it has been without news for longer than it waits. Either way a
// step started now would run unsupervised.
func (r *runner) givenUp() bool {
	if r.w.orphan.Load() {
		return true
	}
	return r.silence > 0 && !r.progress.IsZero() && time.Since(r.progress) > r.silence
}

// step runs fn, which may never return. Exactly one of two things is
// reported for it: its result, if it returns before the deadline, or a
// timeout at the deadline followed, whenever fn returns, by its real
// duration. The mutex makes the choice atomic, so the parent never sees both.
//
// inTime is false when the worker must stop: the step timed out, or the
// parent closed the input and the step was not even started.
func (r *runner) step(step probe.Step, fn func() probe.StepResult) (res probe.StepResult, inTime bool) {
	if r.givenUp() {
		return res, false
	}
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
			if r.onTimeout != nil {
				r.onTimeout()
			}
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
	r.progress = time.Now()
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

	lock, state, err := acquire(v.LockDir, dev.Label(), r.timeout)
	if err != nil {
		r.w.emit(Event{ID: r.id, Type: EventDone, Error: err.Error()})
		return true
	}
	switch state {
	case lockStuck:
		r.w.emit(Event{ID: r.id, Type: EventDone, Stuck: true})
		return true
	case lockBusy:
		r.w.emit(Event{ID: r.id, Type: EventDone, Busy: true})
		return true
	}
	// Held while the steps run: a step that never returns keeps the lock for
	// as long as it blocks, which is the point, and marks the target stuck
	// when it passes its deadline. Released before "done", so that the
	// parent may visit the target again at once.
	r.onTimeout = lock.markStuck
	b.inherit(lock.files())
	alive := r.steps(b, dev, v.Data)
	r.onTimeout = nil
	b.inherit(nil)
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

// dataSteps are the steps of the data probe, in order.
var dataSteps = []probe.Step{probe.StepOpen, probe.StepVerify, probe.StepWrite, probe.StepRead, probe.StepClose}

// data runs the data probe behind the rpc gate. It returns false when a
// step timed out.
func (r *runner) data(b backend, dev target.Device, d Data, rpc probe.StepResult) bool {
	skipFrom := func(i int, why string) {
		for _, s := range dataSteps[i:] {
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
	why := ""
	verify, inTime := r.step(probe.StepVerify, func() probe.StepResult { return f.Verify(d.LfsPath, d.VerifyPin, dev.Index) })
	if !inTime {
		return false
	}
	if verify.Result != probe.ResultOK {
		why = "the file is not verified"
	}
	if why == "" {
		wr, inTime := r.step(probe.StepWrite, func() probe.StepResult { return f.Write() })
		if !inTime {
			return false
		}
		if !wr.Result.Answered() {
			why = fmt.Sprintf("write was %s", wr.Result)
		}
	} else {
		r.skip(probe.StepWrite, why)
	}
	if why == "" {
		if _, inTime := r.step(probe.StepRead, func() probe.StepResult { return f.Read() }); !inTime {
			return false
		}
	} else {
		r.skip(probe.StepRead, why)
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
	// inherit names the files a child process started by a step must keep
	// open: the target's locks.
	inherit([]*os.File)
	close()
}

type dataFile interface {
	Verify(lfsPath string, checkOST bool, want uint32) probe.StepResult
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
func (b *realBackend) inherit(files []*os.File)                     { b.p.Inherit = files }

func (b *realBackend) openData(s probe.DataSpec) (dataFile, probe.StepResult) {
	f, res := b.p.OpenData(s)
	if f == nil {
		// A nil *probe.DataFile must not become a non-nil interface.
		return nil, res
	}
	return f, res
}
