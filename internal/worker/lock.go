// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// A target has two lock files in the lock directory, both empty and both
// locked with flock, so that a lock lasts exactly as long as a process that
// holds it:
//
//	<target>.lock   held during a visit of the target;
//	<target>.stuck  held, in addition, from the moment a step of that visit
//	                passes its deadline until the blocked call returns.
//
// A process that finds the first one held looks at the second: held, the
// target has a blocked probe and must not be probed; free, another process
// is merely visiting the target and will be done soon.
const (
	busySuffix  = ".lock"
	stuckSuffix = ".stuck"
)

// lockPoll is how often a waiting process looks at a busy target again.
const lockPoll = 10 * time.Millisecond

type lockState int

const (
	lockAcquired lockState = iota
	// lockBusy: another process was visiting the target for the whole wait.
	lockBusy
	// lockStuck: another process is blocked in a probe of the target.
	lockStuck
)

// targetLock is the held lock of one target.
type targetLock struct {
	busy, stuck *os.File
}

// openLockFile opens a lock file, creating it. It never follows a symbolic
// link, and the file is private: another user who could open it could lock
// it, and so keep the target from being probed.
func openLockFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
}

func flock(f *os.File, how int) error {
	for {
		err := syscall.Flock(int(f.Fd()), how)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// isHeld reports whether another open file holds a lock on f's file.
func isHeld(f *os.File) (bool, error) {
	err := flock(f, syscall.LOCK_SH|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, flock(f, syscall.LOCK_UN)
}

// acquire takes the lock of one target. While another process is visiting
// the target it waits, up to wait; it does not wait for a blocked probe.
func acquire(dir, label string, wait time.Duration) (*targetLock, lockState, error) {
	if dir == "" {
		return nil, 0, errors.New("no lock directory")
	}
	busy, err := openLockFile(filepath.Join(dir, label+busySuffix))
	if err != nil {
		return nil, 0, fmt.Errorf("lock file: %w", err)
	}
	stuck, err := openLockFile(filepath.Join(dir, label+stuckSuffix))
	if err != nil {
		_ = busy.Close()
		return nil, 0, fmt.Errorf("lock file: %w", err)
	}
	l := &targetLock{busy: busy, stuck: stuck}
	deadline := time.Now().Add(wait)
	for {
		err := flock(busy, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return l, lockAcquired, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			l.release()
			return nil, 0, fmt.Errorf("lock %s: %w", busy.Name(), err)
		}
		held, err := isHeld(stuck)
		if err != nil {
			l.release()
			return nil, 0, fmt.Errorf("lock %s: %w", stuck.Name(), err)
		}
		if held {
			l.release()
			return nil, lockStuck, nil
		}
		if !time.Now().Before(deadline) {
			l.release()
			return nil, lockBusy, nil
		}
		time.Sleep(lockPoll)
	}
}

// markStuck records that a step of the visit passed its deadline. It waits
// for the lock: only the brief looks of isHeld can be in its way.
func (l *targetLock) markStuck() {
	_ = flock(l.stuck, syscall.LOCK_EX)
}

// files are the open lock files. A child process that inherits them keeps
// both locks for as long as it lives, even after this process is gone.
func (l *targetLock) files() []*os.File {
	return []*os.File{l.busy, l.stuck}
}

func (l *targetLock) release() {
	if l == nil {
		return
	}
	if l.busy != nil {
		_ = l.busy.Close()
		l.busy = nil
	}
	if l.stuck != nil {
		_ = l.stuck.Close()
		l.stuck = nil
	}
}

// StuckTargets lists the targets that have a blocked probe, left by any
// process that uses this lock directory. It reads the local lock directory
// only, so the parent process may call it.
func StuckTargets(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		label, ok := strings.CutSuffix(e.Name(), stuckSuffix)
		if !ok || !e.Type().IsRegular() {
			continue
		}
		f, err := os.OpenFile(filepath.Join(dir, e.Name()), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			continue
		}
		held, err := isHeld(f)
		_ = f.Close()
		if err == nil && held {
			out = append(out, label)
		}
	}
	sort.Strings(out)
	return out, nil
}

// PrepareLockDir creates the lock directory if needed and checks that it is
// this user's own and that nobody else can write to it. Lock files are
// created and locked by name: in a directory others can write to, they could
// redirect a lock file or hold a lock for ever.
func PrepareLockDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: cannot read its owner", dir)
	}
	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%s belongs to uid %d, not to this user (uid %d)", dir, st.Uid, os.Geteuid())
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by other users (mode %04o)", dir, fi.Mode().Perm())
	}
	return nil
}
