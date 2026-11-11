# AGENTS.md

## Rules
- `docs/design.md` is the source of truth for the design and its reasons: read the section of what you change first.
- Only a worker process (`internal/worker`, `internal/probe`) touches Lustre: sysfs/procfs/debugfs parameter files, the probe root, `lfs`. The parent never does.
- A probe that passes its deadline is abandoned, never waited for and never killed (an `O_DIRECT` wait ignores `SIGKILL`); one blocked process per target at most, across runs (`<target>.stuck` in the lock directory).
- A worker keeps nothing of its parent: its own session and pipes, no inherited file; a process it starts (`lfs`) inherits the target's lock files.
- A `stuck` target sets the holds its timeout would have set: repeated runs against a hung server must not add blocked processes.
- The worker is the single authority on a step's deadline: exactly one of a result or a `timeout` is reported for a step, never both.
- No data I/O to a target whose `rpc` step did not just get an answer; no write to a file that `verify` did not accept (its OST and layout unless `--no-verify-pin`).
- `timeout` accuses the target: a worker that died, froze or went silent is an `error`, and sets no hold.
- Unknown is not zero: a value that could not be read is absent or `unknown`; only a completed `ok` step is a latency sample, never a corrected one.
- lustremesh creates and deletes nothing under the probe root in `check`; it writes only to this user's own regular file with one link.
- Clean room: no liblustreapi, no ioctl structures, no code or fixtures copied from Lustre (GPL); parameter files, POSIX I/O and the `lfs` command only.
- Dependencies: the standard library and `golang.org/x/sys` only; a new one needs a strong reason and an entry in `THIRD_PARTY_NOTICES.md`.

## Compatibility contracts
- Results (a closed set) and step names: `internal/probe/result.go`; the JSON report: `internal/check/check.go`.
- Probe root layout `<root>/v1/ost/<index>/<client>` and the page a data probe writes: `probe.DataSpec.Path`, `probe.BuildPayload`.
- Lock files `<lock-dir>/<fs>-<OST|MDT><index>.lock` and `.stuck`, and what holding each means: `internal/worker/lock.go`.
- Commands, flags, defaults and exit statuses: `internal/cli/cli.go`, the README flag table.
- Worker protocol (`internal/worker/protocol.go`): parent and worker are the same executable, so it may change freely.

## Before opening a pull request
- `make check`; also `make test-fuzz` for a parser or a name check.
- The watchdog, the locks, the supervisor or the holds: `make test-stress` (their tests, 20 times with the race detector).
- `.github/workflows/`: `make lint-actions` (part of `make check`); after a `uses:` changed, `make lock-actions` (needs a GitHub token) and review the diff of `actions.lock`.
- Behaviour that depends on Lustre: say in the PR whether it ran on a real file system; CI never does (`docs/design.md` §12).

## Git conventions
- Branches: `<type>/<issue>-<summary>` (`<type>/<summary>` without an issue); tools' `agent/*`, `claude/*`, `copilot/*`, `dependabot/*` keep their names.
- Commits and PR titles: Conventional Commits 1.0.0, types `feat` `fix` `perf` `refactor` `test` `docs` `build` `ci` `chore` `revert`; scopes: `probe`, `worker`, `supervisor`, `check`, `target`, `cli`.
