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
| `rpc` | read `/sys/fs/lustre/{osc,mdc}/<device>/kbytesfree`, which sends a statfs RPC to the target and fails with `EAGAIN` instead of waiting when the import is not connected | the read |
| `open` | open this client's file on the OST, without following symbolic links | the final open |
| `pin` | `lfs getstripe -i`: the file's object must be on the OST the probe is attributed to | no |
| `write` | `O_DIRECT` write of one page | the write |
| `read` | `O_DIRECT` read of the page, compared with what was written | the read |
| `close` | close the file | no |

The steps after `rpc` are the data probe. They run for OSTs only, only with
`--probe-root`, and only when the `rpc` step got an answer: no I/O is sent to a
target that did not just answer.

```console
$ lustremesh check --probe-root /mnt/testfs/.lustremesh
TARGET          SERVER        STATE  RPC       OPEN      WRITE      READ      NOTE
testfs-MDT0000  10.0.0.1@tcp  FULL   ok 312µs  -         -          -
testfs-OST0000  10.0.0.2@tcp  IDLE   ok 2.1ms  ok 410µs  ok 1.91ms  ok 640µs
testfs-OST0001  10.0.0.2@tcp  FULL   ok 300µs  ok 400µs  timeout    skipped   write: no answer within 5s; skipped: write timed out

3 targets of testfs, 1 with a step that got no answer; 1 worker(s) still blocked in a probe and left behind
```

### A probe that blocks is abandoned, never waited for

A Lustre client waits for an unreachable target, and since Lustre 2.14 an
`O_DIRECT` call that waits cannot be interrupted, not even by `SIGKILL`. So:

- Only short-lived **worker** processes touch Lustre. The process you start
  only starts workers and prints the report.
- When a step passes `--timeout`, its result is `timeout`, the worker is left
  behind still blocked, and a new worker goes on with the other targets.
- A worker holds a lock file per target (`--lock-dir`) while it probes. A
  target whose worker is still blocked, even from an earlier run, is reported
  as `stuck` and not probed: at most one process is blocked per target.
- A timeout also holds back the probes that would most likely block next: the
  other targets of the same server, and every data probe once the metadata
  path does not answer. At most `--max-abandoned` workers are left blocked.

### Results

| Result | Meaning |
|---|---|
| `ok` | completed and succeeded |
| `timeout` | did not return before the deadline |
| `disconnected` | the `rpc` step found the import not connected |
| `inactive` | the import is deactivated |
| `error` | another failure; the errno is shown |
| `mismatch` | the page read back is not the page just written |
| `contended` | succeeded, but the probing thread waited for a CPU long enough to spoil the measurement |
| `cached` | the `rpc` step was answered by the client's statfs cache: no RPC was sent |
| `stuck` | an earlier probe of this target is still blocked |
| `unprepared` | the file is missing, or is not verified to be on its OST |
| `skipped` | not attempted; the note says why |

Whether an `rpc` step really sent an RPC is known only when the RPC counters
in debugfs are readable (usually by root). Otherwise the report says how many
`rpc` results are unverified.

### Exit status

`0` when every step got an answer (`ok`, `contended` or `cached`), `1` when one
did not or the run could not be carried out, `2` for a command line that cannot
be parsed.

## Preparing the probe files

`check` never creates anything on Lustre. Each client needs one file per OST,
created with a strict OST index (`-o`: with `-i`, Lustre silently uses another
OST when the one asked for is not available), owned by the user who runs
`check`:

```sh
root=/mnt/testfs/.lustremesh
for idx in $(ls /sys/fs/lustre/osc | sed -n 's/.*-OST\([0-9a-f]*\)-osc-[0-9a-f]*$/\1/p' | sort -u); do
    mkdir -p "$root/v1/ost/$idx"
    lfs setstripe -c 1 -o "$((16#$idx))" "$root/v1/ost/$idx/$(hostname)"
done
```

The file name is the client ID: the host name in lower case, or `--client-id`.
The directory name is the OST index as the device name spells it (`0003`).

## Flags

| Flag | Default | |
|---|---|---|
| `--probe-root` | | probe directory on the Lustre mount; without it only `rpc` steps run |
| `--fsname` | | file system name, when several Lustre file systems are mounted |
| `--instance` | | mount instance (hexadecimal suffix of the device names), when the file system is mounted several times |
| `--client-id` | host name | name of this client's files under the probe root |
| `--timeout` | `5s` | deadline of each step |
| `--max-abandoned` | `16` | stop probing when this many workers are blocked |
| `--lock-dir` | `/run/lustremesh` | directory of the per-target lock files, on a local file system |
| `--lfs-path` | `lfs` | the `lfs` command, used to verify the OST of each file |
| `--no-verify-pin` | | write to the files without verifying their OST |
| `--rpc-floor` | `0` | when the RPC counters cannot be read, take an `rpc` step faster than this for a cached answer |
| `--contended-ratio` | `0.1` | a step is `contended` when it waited for a CPU at least this fraction of its duration |
| `--contended-floor` | `50µs` | and at least this long |
| `--sysfs-root` | `/sys/fs/lustre` | |
| `--procfs-root` | `/proc/fs/lustre` | |
| `--debugfs-root` | `/sys/kernel/debug/lustre` | |
| `--json` | | print the report as JSON |

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

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
