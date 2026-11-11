// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package cli

// closeInheritedOnExec does nothing outside Linux, where no probe runs.
func closeInheritedOnExec() {}
