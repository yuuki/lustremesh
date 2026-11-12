// SPDX-License-Identifier: Apache-2.0

//go:build linux

package cli

import (
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// closeInheritedOnExec marks every file this process was started with,
// beyond the three standard ones, close-on-exec. Go opens its own files that
// way, but files passed by the invoker (a lock held by "flock cmd", a pipe of
// a job scheduler) would be inherited by the workers; a worker left blocked
// would keep them open for as long as it is blocked.
func closeInheritedOnExec() {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return
	}
	for _, e := range entries {
		fd, err := strconv.Atoi(e.Name())
		if err != nil || fd < 3 {
			continue
		}
		unix.CloseOnExec(fd)
	}
}
