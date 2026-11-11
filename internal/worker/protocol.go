// SPDX-License-Identifier: Apache-2.0

// Package worker is the only code of lustremesh that touches Lustre.
//
// A worker is a child process that reads requests on its standard input and
// writes events on its standard output, one JSON document per line. It runs
// one step at a time and is the single authority on a step's deadline: when
// the deadline passes it says so itself ("timeout") and goes on blocking,
// because nothing can interrupt it; when the call finally returns it reports
// how long it really took ("late") and exits. The parent never waits for it.
package worker

import (
	"github.com/yuuki/lustremesh/internal/probe"
	"github.com/yuuki/lustremesh/internal/target"
)

// Operations of a Request.
const (
	OpDiscover = "discover"
	OpVisit    = "visit"
)

// Request asks a worker for one operation. The parent sends the next request
// only after the "done" event of the previous one.
type Request struct {
	ID uint64 `json:"id"`
	Op string `json:"op"`
	// StepTimeoutNanos is the deadline of each step.
	StepTimeoutNanos int64        `json:"step_timeout_nanos"`
	Probe            probe.Config `json:"probe"`
	Visit            *Visit       `json:"visit,omitempty"`
}

// Visit is one pass over one target: the rpc probe, then the data probe.
type Visit struct {
	Device target.Device `json:"device"`
	// LockDir holds one lock file per target, on a local file system. The
	// worker keeps the target's lock for the whole visit, so that a blocked
	// worker, even one left behind by an earlier run, keeps others away.
	LockDir string `json:"lock_dir"`
	// Data is nil for a visit with the rpc probe only.
	Data *Data `json:"data,omitempty"`
}

// Data describes the data probe of a visit.
type Data struct {
	Root      string `json:"root"`
	ClientID  string `json:"client_id"`
	Direct    bool   `json:"direct"`
	VerifyPin bool   `json:"verify_pin"`
	LfsPath   string `json:"lfs_path"`
}

// Event types.
const (
	// EventBegin: a step starts; its deadline runs from here.
	EventBegin = "begin"
	// EventStep: a step returned in time (or was skipped by the worker).
	EventStep = "step"
	// EventTimeout: the step named by Step is still blocked at its deadline.
	// The worker sends nothing more for this request except EventLate.
	EventTimeout = "timeout"
	// EventLate: the step that timed out returned after LateNanos.
	EventLate = "late"
	// EventState: the import state read at the start of the rpc step.
	EventState = "state"
	// EventDone: the request is finished.
	EventDone = "done"
)

// Event is what a worker tells its parent.
type Event struct {
	ID   uint64 `json:"id"`
	Type string `json:"type"`
	// Step names the step of EventBegin, EventTimeout and EventLate.
	Step probe.Step `json:"step,omitempty"`
	// Result is the outcome carried by EventStep.
	Result *probe.StepResult `json:"result,omitempty"`
	// LateNanos is the real duration of a step that timed out (EventLate).
	LateNanos int64 `json:"late_nanos,omitempty"`

	// State is the import state read just before the rpc probe (EventState).
	State string `json:"state,omitempty"`

	// On EventDone of a visit:
	// Stuck says that another process holds the target's lock: an earlier
	// probe of this target is still blocked, and nothing was sent.
	Stuck bool `json:"stuck,omitempty"`

	// On EventDone of a discovery:
	Devices []target.Device `json:"devices,omitempty"`
	Skipped []string        `json:"skipped,omitempty"`

	// Error reports a request the worker could not carry out at all.
	Error string `json:"error,omitempty"`
}
