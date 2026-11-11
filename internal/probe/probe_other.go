// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package probe

import (
	"errors"

	"github.com/yuuki/lustremesh/internal/target"
)

// errUnsupported is what every probe reports on a system that is not Linux:
// the package builds there only so that the rest of the code can be worked on.
var errUnsupported = errors.New("lustremesh probes need Linux")

// Prober is a stub outside Linux.
type Prober struct{}

// NewProber returns a stub.
func NewProber(Config) *Prober { return &Prober{} }

// Close does nothing.
func (p *Prober) Close() {}

// ImportState always returns StateUnknown.
func (p *Prober) ImportState(target.Device) string { return StateUnknown }

// RPC always fails.
func (p *Prober) RPC(target.Device) StepResult {
	return StepResult{Step: StepRPC, Result: ResultError, RPCSent: RPCSentUnknown, Detail: errUnsupported.Error()}
}

// DataFile is a stub outside Linux.
type DataFile struct{}

// OpenData always fails.
func (p *Prober) OpenData(DataSpec) (*DataFile, StepResult) {
	return nil, StepResult{Step: StepOpen, Result: ResultError, Detail: errUnsupported.Error()}
}

// Pin always fails.
func (f *DataFile) Pin(string, uint32) StepResult {
	return StepResult{Step: StepPin, Result: ResultUnprepared, Detail: errUnsupported.Error()}
}

// Write always fails.
func (f *DataFile) Write() StepResult {
	return StepResult{Step: StepWrite, Result: ResultError, Detail: errUnsupported.Error()}
}

// Read always fails.
func (f *DataFile) Read() StepResult {
	return StepResult{Step: StepRead, Result: ResultError, Detail: errUnsupported.Error()}
}

// Close does nothing.
func (f *DataFile) Close() StepResult { return StepResult{Step: StepClose, Result: ResultOK} }

// Discover always fails.
func Discover(Config) ([]target.Device, []string, error) {
	return nil, nil, errUnsupported
}
