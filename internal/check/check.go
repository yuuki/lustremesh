// SPDX-License-Identifier: Apache-2.0

// Package check implements "lustremesh check": one visit of every target,
// in an order and under holds that keep a failure from blocking one worker
// per target.
package check

import (
	"context"
	"fmt"

	"github.com/yuuki/lustremesh/internal/probe"
	"github.com/yuuki/lustremesh/internal/supervisor"
	"github.com/yuuki/lustremesh/internal/target"
	"github.com/yuuki/lustremesh/internal/worker"
)

// Visitor is what Run needs of a supervisor.
type Visitor interface {
	Discover(ctx context.Context) ([]target.Device, []string, error)
	Visit(ctx context.Context, dev target.Device, data *worker.Data) supervisor.VisitReport
	Abandoned() int
	Stalls() map[string]supervisor.Stall
}

// Options selects what Run probes.
type Options struct {
	// FSName and Instance choose the mount when several are present.
	FSName   string
	Instance string
	// ProbeRoot is the probe root on the Lustre mount. Empty means rpc
	// probes only.
	ProbeRoot string
	// ClientID names this client's files under ProbeRoot.
	ClientID string
	// VerifyPin checks each file's OST with lfs before writing to it.
	VerifyPin bool
	// LfsPath is the lfs command.
	LfsPath string
	// MaxAbandoned stops the probes when that many workers are blocked.
	MaxAbandoned int
}

// Target is the report of one target.
type Target struct {
	Target    string             `json:"target"`
	Kind      target.Kind        `json:"kind"`
	Index     uint32             `json:"index"`
	Device    string             `json:"device"`
	ServerNID string             `json:"server_nid"`
	State     string             `json:"state"`
	Steps     []probe.StepResult `json:"steps"`
	// Stall is set when a step that timed out returned before the report
	// was written.
	Stall *supervisor.Stall `json:"stall,omitempty"`
}

// Report is the outcome of a run.
type Report struct {
	FS        string   `json:"fs"`
	ClientID  string   `json:"client_id,omitempty"`
	ProbeRoot string   `json:"probe_root,omitempty"`
	Targets   []Target `json:"targets"`
	// Skipped lists entries of the Lustre device directories that are not
	// client imports.
	Skipped []string `json:"skipped_devices,omitempty"`
	// Blocked is the number of worker processes still blocked in a probe
	// when the report was written. They are left behind on purpose.
	Blocked int `json:"blocked_workers"`
}

// Failed reports whether any step did not get an answer from its target.
func (r Report) Failed() bool {
	for _, t := range r.Targets {
		for _, s := range t.Steps {
			if !s.Result.Answered() {
				return true
			}
		}
	}
	return false
}

// Run discovers the targets and visits each once: MDTs first, then OSTs.
//
// Three holds bound the number of workers a single failure can block; each
// lasts until the end of the run, since a one-shot check has no later:
//   - a server whose rpc probe timed out is not probed again;
//   - a server whose data probe timed out gets rpc probes only;
//   - once an MDT did not answer, or an open, pin or close timed out, no data
//     probe is sent at all: every one of them needs the metadata servers.
func Run(ctx context.Context, v Visitor, o Options) (Report, error) {
	rep := Report{ClientID: o.ClientID, ProbeRoot: o.ProbeRoot}
	devs, skipped, err := v.Discover(ctx)
	if err != nil {
		return rep, err
	}
	rep.Skipped = skipped
	sel, err := target.Select(devs, o.FSName, o.Instance)
	if err != nil {
		return rep, err
	}
	rep.FS = sel[0].FS

	var (
		serverHeld = map[string]string{} // server → why nothing is sent to it
		dataHeld   = map[string]string{} // server → why no data probe is sent to it
		metaHeld   string                // why no data probe is sent at all
	)
	for _, dev := range sel {
		var data *worker.Data
		if o.ProbeRoot != "" && dev.Kind == target.KindOST {
			data = &worker.Data{Root: o.ProbeRoot, ClientID: o.ClientID, Direct: true, VerifyPin: o.VerifyPin, LfsPath: o.LfsPath}
		}
		steps := supervisor.Steps(dev, data)
		t := Target{
			Target: dev.Label(), Kind: dev.Kind, Index: dev.Index, Device: dev.Name,
			ServerNID: dev.ServerNID, State: probe.StateUnknown,
		}
		server := serverKey(dev)

		whyNot := ""
		switch {
		case ctx.Err() != nil:
			whyNot = "interrupted"
		case v.Abandoned() >= o.MaxAbandoned:
			whyNot = fmt.Sprintf("%d workers are blocked (--max-abandoned)", v.Abandoned())
		case serverHeld[server] != "":
			whyNot = serverHeld[server]
		}
		if whyNot != "" {
			for _, s := range steps {
				t.Steps = append(t.Steps, probe.Skipped(s, whyNot))
			}
			rep.Targets = append(rep.Targets, t)
			continue
		}

		dataWhyNot := ""
		if data != nil {
			switch {
			case metaHeld != "":
				dataWhyNot = metaHeld
			case dataHeld[server] != "":
				dataWhyNot = dataHeld[server]
			}
		}
		visitData := data
		if dataWhyNot != "" {
			visitData = nil
		}
		vr := v.Visit(ctx, dev, visitData)
		t.State = vr.State
		t.Steps = vr.Steps
		if dataWhyNot != "" {
			for _, s := range steps[len(vr.Steps):] {
				t.Steps = append(t.Steps, probe.Skipped(s, dataWhyNot))
			}
		}
		rep.Targets = append(rep.Targets, t)

		for _, s := range vr.Steps {
			timedOut := s.Result == probe.ResultTimeout
			switch s.Step {
			case probe.StepRPC:
				if timedOut {
					serverHeld[server] = fmt.Sprintf("the rpc probe of %s on the same server timed out", dev.Label())
				}
				if dev.Kind == target.KindMDT && !s.Result.Answered() && metaHeld == "" {
					metaHeld = fmt.Sprintf("%s did not answer (%s)", dev.Label(), s.Result)
				}
			case probe.StepWrite, probe.StepRead:
				if timedOut {
					dataHeld[server] = fmt.Sprintf("%s of %s on the same server timed out", s.Step, dev.Label())
				}
			default: // open, pin, close: the metadata path
				if timedOut && metaHeld == "" {
					metaHeld = fmt.Sprintf("%s of %s timed out", s.Step, dev.Label())
				}
			}
		}
	}

	stalls := v.Stalls()
	for i := range rep.Targets {
		if st, ok := stalls[rep.Targets[i].Target]; ok {
			rep.Targets[i].Stall = &st
		}
	}
	rep.Blocked = v.Abandoned()
	return rep, nil
}

// serverKey groups the targets of one server. A target whose server is not
// known is alone in its group.
func serverKey(dev target.Device) string {
	if dev.ServerNID == "" || dev.ServerNID == target.UnknownNID {
		return "device:" + dev.Name
	}
	return dev.ServerNID
}
