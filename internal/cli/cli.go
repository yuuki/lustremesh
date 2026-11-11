// SPDX-License-Identifier: Apache-2.0

// Package cli is the command line of lustremesh.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/yuuki/lustremesh/internal/check"
	"github.com/yuuki/lustremesh/internal/probe"
	"github.com/yuuki/lustremesh/internal/supervisor"
	"github.com/yuuki/lustremesh/internal/worker"
)

// Version is set at build time (-ldflags "-X .../internal/cli.Version=...").
var Version = "dev"

// Exit statuses.
const (
	ExitOK     = 0 // every step got an answer from its target
	ExitFailed = 1 // a step got no answer, or the run could not be carried out
	ExitUsage  = 2 // the command line cannot be parsed
)

// workerCommand is the hidden subcommand a worker process runs.
const workerCommand = "worker"

const usage = `Usage:
  lustremesh check [flags]   probe every target of a Lustre mount once
  lustremesh version         print the version

Run "lustremesh check --help" for the flags.
`

// Main runs lustremesh with the arguments after the program name.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return ExitUsage
	}
	switch args[0] {
	case "check":
		return runCheck(args[1:], stdout, stderr)
	case workerCommand:
		return worker.Main(stdin, stdout)
	case "version", "--version":
		fmt.Fprintf(stdout, "lustremesh %s\n", Version)
		return ExitOK
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return ExitOK
	default:
		fmt.Fprintf(stderr, "lustremesh: unknown command %q\n\n%s", args[0], usage)
		return ExitUsage
	}
}

// grace is how long a silent worker is given beyond the step timeout.
const grace = 2 * time.Second

// maxTimeout bounds --timeout; a probe that takes an hour has failed.
const maxTimeout = time.Hour

type checkFlags struct {
	probeRoot      string
	fsname         string
	instance       string
	clientID       string
	timeout        time.Duration
	maxAbandoned   int
	lockDir        string
	lfsPath        string
	noVerifyPin    bool
	rpcFloor       time.Duration
	contendedRatio float64
	contendedFloor time.Duration
	sysfsRoot      string
	procfsRoot     string
	debugfsRoot    string
	json           bool
}

func parseCheckFlags(args []string, stderr io.Writer) (checkFlags, error) {
	var f checkFlags
	fs := flag.NewFlagSet("lustremesh check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&f.probeRoot, "probe-root", "", "probe `directory` on the Lustre mount; without it only rpc probes are sent")
	fs.StringVar(&f.fsname, "fsname", "", "file system `name`, when several Lustre file systems are mounted")
	fs.StringVar(&f.instance, "instance", "", "mount instance (`hex` suffix of the device names), when the file system is mounted several times")
	fs.StringVar(&f.clientID, "client-id", "", "`name` of this client's files under the probe root (default: the host name)")
	fs.DurationVar(&f.timeout, "timeout", 5*time.Second, "deadline of each step")
	fs.IntVar(&f.maxAbandoned, "max-abandoned", 16, "stop probing when this many workers are blocked")
	fs.StringVar(&f.lockDir, "lock-dir", "/run/lustremesh", "`directory` of the per-target lock files, on a local file system")
	fs.StringVar(&f.lfsPath, "lfs-path", "lfs", "the lfs `command`, used to verify the OST of each file")
	fs.BoolVar(&f.noVerifyPin, "no-verify-pin", false, "write to the files without verifying their OST and layout with lfs")
	fs.DurationVar(&f.rpcFloor, "rpc-floor", 0, "when the RPC counters cannot be read, take an rpc probe faster than this for a cached answer (0: never)")
	fs.Float64Var(&f.contendedRatio, "contended-ratio", 0.1, "a probe is contended when it waited for a CPU at least this fraction of its duration")
	fs.DurationVar(&f.contendedFloor, "contended-floor", 50*time.Microsecond, "and at least this long")
	fs.StringVar(&f.sysfsRoot, "sysfs-root", "/sys/fs/lustre", "Lustre `directory` in sysfs")
	fs.StringVar(&f.procfsRoot, "procfs-root", "/proc/fs/lustre", "Lustre `directory` in procfs")
	fs.StringVar(&f.debugfsRoot, "debugfs-root", "/sys/kernel/debug/lustre", "Lustre `directory` in debugfs")
	fs.BoolVar(&f.json, "json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	if fs.NArg() > 0 {
		return f, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	switch {
	case f.timeout <= 0 || f.timeout > maxTimeout:
		return f, fmt.Errorf("--timeout must be positive and at most %s", maxTimeout)
	case f.maxAbandoned < 1:
		return f, errors.New("--max-abandoned must be at least 1")
	case f.rpcFloor < 0:
		return f, errors.New("--rpc-floor must not be negative")
	case f.contendedRatio <= 0 || f.contendedRatio > 1:
		return f, errors.New("--contended-ratio must be in (0, 1]")
	case f.contendedFloor < 0:
		return f, errors.New("--contended-floor must not be negative")
	case f.lockDir == "":
		return f, errors.New("--lock-dir must not be empty")
	case f.sysfsRoot == "":
		return f, errors.New("--sysfs-root must not be empty")
	}
	if f.clientID != "" && !probe.ValidClientID(f.clientID) {
		return f, fmt.Errorf("--client-id %q: use letters, digits, '.', '_' and '-' only", f.clientID)
	}
	return f, nil
}

// syncWriter serializes writes of this process and of the goroutines that
// copy the workers' standard error.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func runCheck(args []string, stdout, stderr io.Writer) int {
	// The workers' standard error is copied here from other goroutines.
	stderr = &syncWriter{w: stderr}
	closeInheritedOnExec()
	f, err := parseCheckFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		fmt.Fprintf(stderr, "lustremesh check: %v\n", err)
		return ExitUsage
	}
	if f.clientID == "" && f.probeRoot != "" {
		host, err := os.Hostname()
		if err == nil {
			f.clientID = probe.NormalizeClientID(host)
		}
		if f.clientID == "" {
			fmt.Fprintln(stderr, "lustremesh check: cannot derive a client ID from the host name: pass --client-id")
			return ExitFailed
		}
	}
	// The lock directory is local, so the parent may create and check it.
	// Everything on Lustre, the probe root included, is left to the workers.
	if err := worker.PrepareLockDir(f.lockDir); err != nil {
		fmt.Fprintf(stderr, "lustremesh check: lock directory: %v\n"+
			"create it for this user (mode 0700, on a local file system), or choose another with --lock-dir\n", err)
		return ExitFailed
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "lustremesh check: cannot find this executable to start workers: %v\n", err)
		return ExitFailed
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sup := supervisor.New(supervisor.Config{
		Command: func() *exec.Cmd {
			cmd := exec.Command(exe, workerCommand) //nolint:noctx // a worker must outlive any context
			// A session of its own: a Ctrl-C or a hang-up meant for this
			// process must not reach a worker, whose death would free the
			// target's lock while a child of it may still be blocked.
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			return cmd
		},
		Stderr:      stderr,
		StepTimeout: f.timeout,
		Grace:       grace,
		LockDir:     f.lockDir,
		Probe: probe.Config{
			SysfsRoot:           f.sysfsRoot,
			ProcfsRoot:          f.procfsRoot,
			DebugfsRoot:         f.debugfsRoot,
			ContendedRatio:      f.contendedRatio,
			ContendedFloorNanos: int64(f.contendedFloor),
			RPCFloorNanos:       int64(f.rpcFloor),
		},
	})
	defer sup.Close()

	rep, err := check.Run(ctx, sup, check.Options{
		FSName:       f.fsname,
		Instance:     f.instance,
		ProbeRoot:    f.probeRoot,
		ClientID:     f.clientID,
		VerifyPin:    !f.noVerifyPin,
		LfsPath:      f.lfsPath,
		MaxAbandoned: f.maxAbandoned,
		StuckTargets: func() []string {
			stuck, _ := worker.StuckTargets(f.lockDir)
			return stuck
		},
	})
	if err != nil {
		fmt.Fprintf(stderr, "lustremesh check: %v\n", err)
		return ExitFailed
	}
	write := check.WriteText
	if f.json {
		write = check.WriteJSON
	}
	if err := write(stdout, rep); err != nil {
		fmt.Fprintf(stderr, "lustremesh check: %v\n", err)
		return ExitFailed
	}
	if rep.Failed() || ctx.Err() != nil {
		return ExitFailed
	}
	return ExitOK
}
