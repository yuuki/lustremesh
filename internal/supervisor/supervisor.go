// SPDX-License-Identifier: Apache-2.0

// Package supervisor runs worker processes and never touches Lustre itself.
//
// One worker at a time carries out the requests. When a worker reports that
// a step passed its deadline, the supervisor gives it up without waiting
// (docs/design.md §6): the worker stays blocked for as long as the kernel
// keeps it, a new worker takes over, and the number of workers given up and
// still alive is bounded by the caller through Abandoned.
package supervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/yuuki/lustremesh/internal/probe"
	"github.com/yuuki/lustremesh/internal/target"
	"github.com/yuuki/lustremesh/internal/worker"
)

// Config configures a Supervisor.
type Config struct {
	// Command builds the command of a new worker process. The supervisor
	// sets its standard streams.
	Command func() *exec.Cmd
	// StepTimeout is the deadline of each step, enforced by the worker.
	StepTimeout time.Duration
	// Grace is how much longer than StepTimeout the supervisor waits for a
	// worker that says nothing before giving it up as well.
	Grace time.Duration
	// Probe is passed to the workers.
	Probe probe.Config
	// LockDir is the directory of the per-target lock files.
	LockDir string
	// Stderr receives what the workers write to their standard error. It
	// is written to from several goroutines. Nil discards it.
	Stderr io.Writer
}

// Stall is the real duration of a step that timed out and returned later.
type Stall struct {
	Step  probe.Step `json:"step"`
	Nanos int64      `json:"nanos"`
}

// VisitReport is the outcome of one visit.
type VisitReport struct {
	// State is the import state before the rpc probe, or "unknown".
	State string
	// Steps has one result for each step of the visit, in order.
	Steps []probe.StepResult
}

// Supervisor owns the worker processes of one run.
type Supervisor struct {
	cfg Config

	cur    *proc
	nextID uint64

	mu        sync.Mutex
	abandoned int
	stalls    map[string]Stall
}

// New returns a Supervisor. No process is started before the first request.
func New(cfg Config) *Supervisor {
	return &Supervisor{cfg: cfg, stalls: map[string]Stall{}}
}

type proc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	events chan worker.Event
}

func (s *Supervisor) start() (*proc, error) {
	cmd := s.cfg.Command()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// A worker gets a pipe of its own for its standard error, never this
	// process's: a worker left blocked would otherwise keep that file open,
	// and whoever waits for it to close (a shell pipeline, ssh, cron) would
	// wait as long as the worker is blocked.
	errR, errW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = errW
	err = cmd.Start()
	_ = errW.Close()
	if err != nil {
		_ = errR.Close()
		return nil, fmt.Errorf("start worker: %w", err)
	}
	go func() {
		defer func() { _ = errR.Close() }()
		w := s.cfg.Stderr
		if w == nil {
			w = io.Discard
		}
		_, _ = io.Copy(w, errR)
	}()
	p := &proc{cmd: cmd, stdin: stdin, events: make(chan worker.Event, 16)}
	go func() {
		defer close(p.events)
		dec := json.NewDecoder(bufio.NewReader(stdout))
		for {
			var ev worker.Event
			if err := dec.Decode(&ev); err != nil {
				return
			}
			p.events <- ev
		}
	}()
	return p, nil
}

// abandon gives a worker up: its input is closed, which tells it to start
// nothing more, and it is neither killed (a blocked O_DIRECT call ignores
// SIGKILL) nor waited for. If it reports later how long its blocked step
// took, the stall is kept under label.
func (s *Supervisor) abandon(p *proc, label string) {
	_ = p.stdin.Close()
	s.mu.Lock()
	s.abandoned++
	s.mu.Unlock()
	go func() {
		for ev := range p.events {
			if ev.Type == worker.EventLate && label != "" {
				s.mu.Lock()
				s.stalls[label] = Stall{Step: ev.Step, Nanos: ev.LateNanos}
				s.mu.Unlock()
			}
		}
		_ = p.cmd.Wait()
		s.mu.Lock()
		s.abandoned--
		s.mu.Unlock()
	}()
}

// reap collects a worker that ended by itself.
func reap(p *proc) {
	_ = p.stdin.Close()
	go func() {
		for range p.events { //nolint:revive // drain until the pipe closes
		}
		_ = p.cmd.Wait()
	}()
}

// Abandoned is the number of workers given up that are still alive.
func (s *Supervisor) Abandoned() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.abandoned
}

// Stalls returns, by target label, the steps that timed out and have
// returned since.
func (s *Supervisor) Stalls() map[string]Stall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Stall, len(s.stalls))
	for k, v := range s.stalls {
		out[k] = v
	}
	return out
}

// Close ends the current worker. Workers given up are left alone.
func (s *Supervisor) Close() {
	if s.cur != nil {
		reap(s.cur)
		s.cur = nil
	}
}

// outcome is how a request ended.
type outcome struct {
	done  *worker.Event // the worker's "done" event
	state string        // the import state the worker read, if any
	// Otherwise the request broke at step (empty when no step had begun):
	timedOut bool   // the worker reported that the step passed its deadline
	failure  string // the worker died or went silent, or the run was canceled
	step     probe.Step
	// undelivered: the worker was gone before it saw the request. Nothing
	// was sent to Lustre, so the request may be given to another worker.
	undelivered bool
}

// run sends one request and follows it to its end. onStep sees the result of
// each step that returned in time. A request that found its worker dead is
// given once to a fresh worker: a worker that died between two requests says
// nothing about the target of the second.
func (s *Supervisor) run(ctx context.Context, req worker.Request, label string, onStep func(probe.StepResult)) outcome {
	out := s.attempt(ctx, req, label, onStep)
	if out.undelivered {
		out = s.attempt(ctx, req, label, onStep)
	}
	return out
}

func (s *Supervisor) attempt(ctx context.Context, req worker.Request, label string, onStep func(probe.StepResult)) outcome {
	if s.cur == nil {
		p, err := s.start()
		if err != nil {
			return outcome{failure: err.Error()}
		}
		s.cur = p
	}
	p := s.cur
	s.nextID++
	req.ID = s.nextID
	req.StepTimeoutNanos = int64(s.cfg.StepTimeout)
	req.Probe = s.cfg.Probe
	// The worker enforces the deadline; the silence timer only catches a
	// worker that stopped talking altogether. Waiting for a busy target's
	// lock is the longest a healthy worker stays silent: one step timeout.
	silence := s.cfg.StepTimeout + s.cfg.Grace
	req.SentUnixNanos = time.Now().UnixNano()
	req.SilenceNanos = int64(silence)

	line, err := json.Marshal(req)
	if err != nil {
		return outcome{failure: "encode request: " + err.Error()}
	}
	if _, err := p.stdin.Write(append(line, '\n')); err != nil {
		s.cur = nil
		reap(p)
		return outcome{failure: "the worker is gone: " + err.Error(), undelivered: true}
	}

	timer := time.NewTimer(silence)
	defer timer.Stop()
	var (
		current probe.Step
		state   string
		heard   bool
	)
	for {
		select {
		case ev, ok := <-p.events:
			if !ok {
				s.cur = nil
				reap(p)
				return outcome{failure: "the worker exited", step: current, state: state, undelivered: !heard}
			}
			if ev.ID != req.ID {
				continue
			}
			heard = true
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(silence)
			switch ev.Type {
			case worker.EventBegin:
				current = ev.Step
			case worker.EventState:
				state = ev.State
			case worker.EventStep:
				if ev.Result != nil {
					onStep(*ev.Result)
				}
				current = ""
			case worker.EventTimeout:
				s.cur = nil
				s.abandon(p, label)
				return outcome{timedOut: true, step: ev.Step, state: state}
			case worker.EventDone:
				return outcome{done: &ev, state: state}
			}
		case <-timer.C:
			// Not a timeout of the target: no step is known to have passed
			// its deadline, the worker itself is frozen or starved. It is
			// given up all the same, and its closed input keeps it from
			// starting another step if it comes back.
			s.cur = nil
			s.abandon(p, label)
			return outcome{failure: fmt.Sprintf("the worker said nothing for %s", silence), step: current, state: state}
		case <-ctx.Done():
			s.cur = nil
			s.abandon(p, "")
			return outcome{failure: "interrupted", step: current, state: state}
		}
	}
}

// Discover asks a worker for the client's import devices.
func (s *Supervisor) Discover(ctx context.Context) (devs []target.Device, skipped []string, err error) {
	out := s.run(ctx, worker.Request{Op: worker.OpDiscover}, "", func(probe.StepResult) {})
	switch {
	case out.timedOut:
		return nil, nil, fmt.Errorf("listing the Lustre devices did not return within %s", s.cfg.StepTimeout)
	case out.failure != "":
		return nil, nil, errors.New(out.failure)
	case out.done.Error != "":
		return nil, nil, errors.New(out.done.Error)
	}
	return out.done.Devices, out.done.Skipped, nil
}

// Steps lists the steps of a visit, in order.
func Steps(dev target.Device, data *worker.Data) []probe.Step {
	if data == nil || dev.Kind != target.KindOST {
		return []probe.Step{probe.StepRPC}
	}
	return []probe.Step{probe.StepRPC, probe.StepOpen, probe.StepVerify, probe.StepWrite, probe.StepRead, probe.StepClose}
}

// Visit probes one target. data is nil for the rpc probe only. The report
// always has a result for every step of the visit: what the worker did not
// report is filled in with the reason.
func (s *Supervisor) Visit(ctx context.Context, dev target.Device, data *worker.Data) VisitReport {
	want := Steps(dev, data)
	rep := VisitReport{State: probe.StateUnknown}
	got := map[probe.Step]probe.StepResult{}
	out := s.run(ctx, worker.Request{
		Op:    worker.OpVisit,
		Visit: &worker.Visit{Device: dev, LockDir: s.cfg.LockDir, Data: data},
	}, dev.Label(), func(r probe.StepResult) { got[r.Step] = r })

	// brokenAt is the step at which a request that did not finish broke:
	// the one the worker had begun, or else the first one it did not report.
	brokenAt := out.step
	if out.done == nil && brokenAt == "" {
		for _, step := range want {
			if _, ok := got[step]; !ok {
				brokenAt = step
				break
			}
		}
	}
	fill := func(step probe.Step) probe.StepResult {
		switch {
		case out.done != nil && out.done.Stuck:
			return probe.StepResult{Step: step, Result: probe.ResultStuck, Detail: "an earlier probe of this target is still blocked"}
		case out.done != nil && out.done.Busy:
			return probe.Skipped(step, "another lustremesh process is probing this target")
		case out.done != nil && out.done.Error != "":
			return probe.StepResult{Step: step, Result: probe.ResultError, Detail: out.done.Error}
		case out.done != nil:
			return probe.StepResult{Step: step, Result: probe.ResultError, Detail: "the worker reported nothing for this step"}
		case step == brokenAt && out.timedOut:
			return probe.StepResult{Step: step, Result: probe.ResultTimeout, Detail: fmt.Sprintf("no answer within %s", s.cfg.StepTimeout)}
		case step == brokenAt:
			return probe.StepResult{Step: step, Result: probe.ResultError, Detail: out.failure}
		case out.timedOut:
			return probe.Skipped(step, fmt.Sprintf("%s timed out", brokenAt))
		default:
			return probe.Skipped(step, out.failure)
		}
	}
	if out.state != "" {
		rep.State = out.state
	}
	for _, step := range want {
		if r, ok := got[step]; ok {
			rep.Steps = append(rep.Steps, r)
			continue
		}
		rep.Steps = append(rep.Steps, fill(step))
	}
	return rep
}
