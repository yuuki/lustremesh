# lustremesh

lustremesh probes each OST and MDT of a Lustre file system from the clients,
with small fixed-shape operations, to tell whether the I/O path to every target
works and how long it takes: the Pingmesh idea applied to storage.

**Status: milestone M0.** Only `lustremesh check` exists: it probes every
target once and prints a table. The agent, the metrics and the fault
localization are not written yet; [docs/design.md](docs/design.md) (in
Japanese) has the design and the plan.

**Not yet run against a real Lustre file system.** The behaviour of Lustre
that the code relies on comes from reading the Lustre sources (2.12 to 2.17)
and the Operations Manual; the tests use fake parameter trees. The list of
what must be confirmed on a real system is in `docs/design.md` §12.

## What `check` does

For every target of the mount, MDTs first:

| Step | Operation | Timed |
|---|---|---|
| `rpc` | read `/sys/fs/lustre/{osc,mdc}/<device>/kbytesfree`, which sends a statfs RPC to the target | the read |
| `open` | open this client's file on the OST, without following symbolic links | the final open |
| `verify` | the file must be this user's own regular file with one link and, with `lfs getstripe`, have a plain layout of one stripe on the OST the probe is attributed to | no |
| `write` | `O_DIRECT` write of one page | the write |
| `read` | `O_DIRECT` read of the page, compared with what was written | the read |
| `close` | close the file | no |

The steps after `rpc` are the data probe. They run for OSTs only, only with
`--probe-root`, and only when the `rpc` step got an answer: no I/O is sent to a
target that did not just answer.

The `rpc` step does not wait for an import that Lustre knows to be
disconnected: the read fails with `EAGAIN` (`disconnected`). It does wait for an
idle import to reconnect, so the first probe of a server that went down while
the client was idle blocks, and is a `timeout`.

```console
$ lustremesh check --probe-root /mnt/testfs/.lustremesh
TARGET          SERVER        STATE  RPC       OPEN      WRITE      READ      NOTE
testfs-MDT0000  10.0.0.1@tcp  FULL   ok 312µs  -         -          -
testfs-OST0000  10.0.0.2@tcp  IDLE   ok 2.1ms  ok 410µs  ok 1.91ms  ok 640µs
testfs-OST0001  10.0.0.2@tcp  FULL   ok 300µs  ok 400µs  timeout    skipped   write: no answer within 5s; skipped: write timed out

3 targets of testfs, 1 with a step that got no answer; 1 worker(s) still blocked in a probe and left behind
1 target(s) with a probe still blocked, from this run or an earlier one: testfs-OST0001
```

### A probe that blocks is abandoned, never waited for

A Lustre client waits for an unreachable target, and since Lustre 2.14 an
`O_DIRECT` call that waits cannot be interrupted, not even by `SIGKILL`. So:

- Only **worker** processes touch Lustre. The process you start only starts
  workers and prints the report.
- When a step passes `--timeout`, its result is `timeout`, the worker is left
  behind still blocked, and a new worker goes on with the other targets.
  `check` returns without it: a worker has its own session and its own pipes,
  and keeps none of the files `check` was started with.
- Two lock files per target (`--lock-dir`) say what is going on. A target that
  another run is visiting is waited for, up to `--timeout`. A target with a
  probe still blocked, even from an earlier run, is `stuck`: it is not probed,
  so at most one process is blocked per target.
- A timeout, or a `stuck` target, also holds back the probes that would most
  likely block next: the other targets of the same server, and every data
  probe once the metadata path does not answer. `check` stops probing when
  `--max-abandoned` probes are blocked, counting those of earlier runs.

So `check` can be run again and again against a hung server, from cron for
instance, without adding a blocked process each time.

### Results

| Result | Meaning |
|---|---|
| `ok` | completed and succeeded |
| `timeout` | did not return before the deadline |
| `disconnected` | the `rpc` step found the import not connected |
| `inactive` | the import is deactivated |
| `error` | another failure; the errno or the note says which |
| `mismatch` | the page read back is not the page just written |
| `contended` | succeeded, but the probing thread waited for a CPU long enough to spoil the measurement |
| `cached` | the `rpc` step was answered by the client's statfs cache: no RPC was sent |
| `stuck` | a probe of this target is still blocked, from this run or an earlier one |
| `unprepared` | the file is missing, or is not verified |
| `skipped` | not attempted; the note says why |

### Exit status

`0` when every step got an answer (`ok`, `contended` or `cached`), `1` when one
did not or the run could not be carried out, `2` for a command line that cannot
be parsed.

## Setting up

Run lustremesh as one dedicated, unprivileged user on every client.

**The lock directory** must exist on a local file system, belong to that user
and be writable by nobody else; `check` creates it when it can. The default,
`/run/lustremesh`, is under a directory only root can write to, so create it
at boot, for example with `/etc/tmpfiles.d/lustremesh.conf`:

```
d /run/lustremesh 0700 lustremesh lustremesh -
```

The locks only exclude processes that use the same lock directory.

**The probe files.** `check` never creates anything on Lustre. Each client
needs one file per OST, created with a strict OST index (`-o`: with `-i`,
Lustre silently uses another OST when the one asked for is not available) and
owned by the user who runs `check`:

```sh
fs=testfs
root=/mnt/$fs/.lustremesh
client=$(hostname | tr '[:upper:]' '[:lower:]')
for idx in $(ls /sys/fs/lustre/osc | sed -n "s/^$fs-OST\([0-9a-f]*\)-osc-[0-9a-f]*\$/\1/p" | sort -u); do
    mkdir -p "$root/v1/ost/$idx"
    lfs setstripe -c 1 -o "$((16#$idx))" "$root/v1/ost/$idx/$client"
done
```

The file name is the client ID: the host name in lower case, or `--client-id`.
It must be unique among the clients of the file system. The directory name is
the OST index as the device name spells it (`0003`). The probe root must not
have a default layout that is composite or keeps data on the MDT: such a file
is refused as `unprepared`.

## Flags

| Flag | Default | |
|---|---|---|
| `--probe-root` | | probe directory on the Lustre mount; without it only `rpc` steps run |
| `--fsname` | | file system name, when several Lustre file systems are mounted |
| `--instance` | | mount instance (hexadecimal suffix of the device names), when the file system is mounted several times |
| `--client-id` | host name | name of this client's files under the probe root |
| `--timeout` | `5s` | deadline of each step, at most `1h` |
| `--max-abandoned` | `16` | stop probing when this many probes are blocked |
| `--lock-dir` | `/run/lustremesh` | directory of the per-target lock files, on a local file system |
| `--lfs-path` | `lfs` | the `lfs` command, used to verify the OST and layout of each file |
| `--no-verify-pin` | | write to the files without verifying their OST and layout |
| `--rpc-floor` | `0` | when the RPC counters cannot be read, take an `rpc` step faster than this for a cached answer |
| `--contended-ratio` | `0.1` | a step is `contended` when it waited for a CPU at least this fraction of its duration |
| `--contended-floor` | `50µs` | and at least this long |
| `--sysfs-root` | `/sys/fs/lustre` | |
| `--procfs-root` | `/proc/fs/lustre` | |
| `--debugfs-root` | `/sys/kernel/debug/lustre` | |
| `--json` | | print the report as JSON |

## Limits to know

- **Whether an `rpc` step sent an RPC** is known only when the RPC counters in
  debugfs are readable (usually by root), and not for the first statfs after a
  mount. Otherwise the report says how many `rpc` results are unverified. A
  cached answer is up to about two seconds old, and the data probe is sent
  behind it all the same.
- **`contended` sees the probing thread only.** The Lustre kernel threads that
  send and complete an RPC compete with the jobs for CPUs too; when they are
  starved, the sample is inflated and still reported `ok`.
- **Install the binary on a local file system.** `check` starts workers by
  executing itself; from Lustre, that could block.
- **A worker waiting for `lfs`** can be killed while `lfs` stays blocked; the
  target then stays locked until `lfs` returns, and other runs report it as
  being probed (`skipped`), not `stuck`.

## Build

```sh
make build   # ./lustremesh, static, no cgo
make check   # lint, tests with the race detector, build
```

lustremesh uses no Lustre library and no ioctl: it reads parameter files, does
ordinary file I/O and runs `lfs`. Linux only; it builds elsewhere so that the
code can be worked on.

## Prior art

The closest work is the "Store Pings" of Kaleidoscope (Jha et al., SC20), which
probed the OSTs of Blue Waters from six monitors. lustremesh aims at probing
from every client under a load budget, as open source, with Prometheus
metrics; see `docs/design.md` §2.

## License

Apache-2.0. See [LICENSE](LICENSE), [NOTICE](NOTICE) and
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
