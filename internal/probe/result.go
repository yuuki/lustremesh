// SPDX-License-Identifier: Apache-2.0

// Package probe implements the probes a worker process sends to one Lustre
// target, and the parsers of the files they read.
//
// Everything here may block for as long as a target is unreachable (an
// O_DIRECT write cannot even be killed), so it runs only in a worker process,
// under the worker's watchdog (docs/design.md §6).
package probe

import (
	"path/filepath"
	"time"
)

// Result is the outcome of one step; the set is closed (docs/design.md §8.1).
type Result string

const (
	// ResultOK: the operation completed and succeeded.
	ResultOK Result = "ok"
	// ResultTimeout: the operation did not return before its deadline.
	ResultTimeout Result = "timeout"
	// ResultDisconnected: the rpc probe found the import not connected.
	ResultDisconnected Result = "disconnected"
	// ResultInactive: the import is deactivated.
	ResultInactive Result = "inactive"
	// ResultError: any other failure; Errno or Detail says which.
	ResultError Result = "error"
	// ResultMismatch: the bytes read back differ from the bytes just written.
	ResultMismatch Result = "mismatch"
	// ResultContended: the operation succeeded, but the probing thread waited
	// for a CPU long enough to spoil the measurement.
	ResultContended Result = "contended"
	// ResultCached: the rpc probe was answered from the client's statfs cache
	// and sent no RPC.
	ResultCached Result = "cached"
	// ResultStuck: an earlier probe of this target is still blocked.
	ResultStuck Result = "stuck"
	// ResultUnprepared: the pinned file is missing or is not what it must be.
	ResultUnprepared Result = "unprepared"
	// ResultSkipped: the step was not attempted; Detail says why.
	ResultSkipped Result = "skipped"
)

// Answered reports whether the target answered the operation: the result is a
// success, even if its duration is not a usable sample.
func (r Result) Answered() bool {
	return r == ResultOK || r == ResultContended || r == ResultCached
}

// Step names one operation of a visit, or the discovery of the targets.
type Step string

const (
	StepDiscover Step = "discover"
	StepRPC      Step = "rpc"
	StepOpen     Step = "open"
	StepPin      Step = "pin"
	StepWrite    Step = "write"
	StepRead     Step = "read"
	StepClose    Step = "close"
)

// RPCSent tells whether the rpc probe is known to have sent an RPC.
type RPCSent string

const (
	RPCSentYes     RPCSent = "yes"
	RPCSentNo      RPCSent = "no"
	RPCSentUnknown RPCSent = "unknown"
)

// StepResult is what one step reports.
type StepResult struct {
	Step   Step   `json:"step"`
	Result Result `json:"result"`
	// Nanos is the duration of the timed system call, when one completed.
	// Only a ResultOK duration is a latency sample.
	Nanos *int64 `json:"nanos,omitempty"`
	// RunDelayNanos is how long the probing thread waited for a CPU during
	// the timed call; nil when the kernel does not tell.
	RunDelayNanos *int64 `json:"run_delay_nanos,omitempty"`
	// Errno is the symbolic error number of a failed system call.
	Errno string `json:"errno,omitempty"`
	// Detail explains a result that is not ResultOK.
	Detail string `json:"detail,omitempty"`
	// RPCSent is set on the rpc step only.
	RPCSent RPCSent `json:"rpc_sent,omitempty"`
}

// Skipped builds the result of a step that is not attempted.
func Skipped(step Step, why string) StepResult {
	return StepResult{Step: step, Result: ResultSkipped, Detail: why}
}

// Config holds the paths and thresholds of the probes.
type Config struct {
	// SysfsRoot is /sys/fs/lustre.
	SysfsRoot string `json:"sysfs_root"`
	// ProcfsRoot is /proc/fs/lustre, where Lustre up to 2.16 keeps the
	// import state.
	ProcfsRoot string `json:"procfs_root"`
	// DebugfsRoot is /sys/kernel/debug/lustre, where the RPC counters are
	// (and the import state from Lustre 2.17 on). Usually readable by root
	// only; the probes work without it.
	DebugfsRoot string `json:"debugfs_root"`
	// ContendedRatio and ContendedFloorNanos define ResultContended: the
	// run-queue wait is at least the floor and at least the ratio of the
	// measured duration.
	ContendedRatio      float64 `json:"contended_ratio"`
	ContendedFloorNanos int64   `json:"contended_floor_nanos"`
	// RPCFloorNanos classifies an rpc probe as ResultCached when the RPC
	// counters cannot be read and the read returned faster than this. Zero
	// disables the rule: such a probe is ResultOK with RPCSentUnknown.
	RPCFloorNanos int64 `json:"rpc_floor_nanos"`
}

// Contended applies the rule of Config to one measurement.
func (c Config) Contended(elapsed, runDelay time.Duration) bool {
	if runDelay <= 0 || int64(runDelay) < c.ContendedFloorNanos {
		return false
	}
	return float64(runDelay) >= c.ContendedRatio*float64(elapsed)
}

// DataSpec names the file of a data probe.
type DataSpec struct {
	// Root is the probe root on the Lustre mount.
	Root string
	// IndexHex is the OST index as the device name spells it.
	IndexHex string
	// ClientID names this client's file.
	ClientID string
	// Direct opens the file with O_DIRECT. Always true outside the tests,
	// whose temporary directory may not support it.
	Direct bool
}

// Path is where the client's file for an OST lives under the probe root:
// <root>/v1/ost/<index>/<client>. The layout is a compatibility contract.
func (s DataSpec) Path() string {
	return filepath.Join(s.Root, "v1", "ost", s.IndexHex, s.ClientID)
}
