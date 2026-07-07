# Parallel Forking Runtime Bugs: Root Causes and Fixes

This note records the Linux runtime failures found while validating concurrent fork
execution, the experiments used to isolate them, and the fixes now in the worktree.
It is intended as pickup context for future agents.

## 2026-07-06 Addendum: True Root Causes — Long-Lived Shell Is Viable

Later root-cause work (see "Definitive Root Causes" at the end of this doc) proved that
Bug 4 below was **not** caused by the long-lived shell architecture. It was caused by
CRIU < 4.0 not checkpointing ARM64 pointer-authentication (PAC) keys. The ephemeral
`bash -lc` redesign described in Bugs 3/4 is therefore an unnecessary workaround that
sacrifices persistent shell state (cwd, variables, background jobs, REPLs) for nothing.

The validated fix set for the long-lived PTY shell model is:

1. `cmd.Process.Release()` in `bash_init` after starting bash (fixes Bug 2's pidfd).
2. CRIU >= 4.0 on aarch64 hosts with PAC (`paca`/`pacg` in `/proc/cpuinfo`); PAC key
   C/R landed in criu PR #2609 (merged 2025-03-15) plus #2712. Fedora 38 ships CRIU
   3.18, which predates it — build v4.2 from source.

With those two changes, the full matrix passes with the persistent shell: shell
variables, cwd, and running background jobs survive checkpoint, restore, forking, and
recursive snapshot; forks diverge independently. Do not merge the ephemeral-exec
commit; keep the PTY server.

Related architecture context:

- `AGENTS.md`
- `parallel-forking-design.md`
- `parallel-forking-mvp-plan.md`
- `parallel-forking-implementation-analysis.md`

## Environment

Runtime validation was run from:

```text
/home/fedora/.codex/worktrees/5cd0/waypoint
```

Build/test commands:

```bash
env GOCACHE=/tmp/waypoint-go-cache go test ./...
env CGO_ENABLED=0 GOCACHE=/tmp/waypoint-go-cache go build -o bash_init ./cmd/bash-init
env GOCACHE=/tmp/waypoint-go-cache go build -o waypoint ./cmd/waypoint
sudo criu check
```

`sudo criu check` returned:

```text
Looks good.
```

Runtime tests used disposable rootfs directories under `/tmp` and cleaned sessions with:

```bash
sudo ./waypoint cleanup <session> --force
sudo findmnt -R /tmp/waypoint-sessions/<session>
```

No leftover Waypoint mounts were present after the final validation run.

## Fixed Bug 1: Minimal Rootfs `exec` Appeared to Hang

### Symptom

`waypoint init <rootfs> --shell --quiet` succeeded, but the first exec could hang or
return no useful output:

```bash
sudo env WAYPOINT_BASH_INIT_SRC="$PWD/bash_init" ./waypoint init "$ROOTFS" --shell --quiet
timeout 20 sudo ./waypoint exec "$SESSION" main -- "echo hi"
```

### Root Cause

The minimal rootfs had `/bin/bash`, `libc`, and the dynamic loader, but was missing other
bash runtime dependencies such as `libtinfo.so.6`.

The old startup readiness check only proved that `bash_init` created its Unix socket. It
did not prove that the child bash was usable. Bash could fail after the socket appeared,
leaving later exec calls waiting for command output that would never arrive.

### Fix

`Manager.StartShell` now stages runtime dependencies for the rootfs bash before starting
`bash_init`:

```go
if err := stageRootBinaryRuntimeDeps(workDir, "/bin/bash"); err != nil {
    return ShellNotEnabled, "", fmt.Errorf("failed to stage bash runtime dependencies: %w", err)
}
```

The helper runs `ldd` on the rootfs binary and copies any host-resolved shared libraries
into blank paths in the rootfs.

### Validation

A deliberately incomplete rootfs containing only bash, libc, and the loader now works:

```text
EXEC_OUTPUT=healed-bash-deps
```

## Fixed Bug 2: CRIU Dump Failed on `anon_inode:[pidfd]`

### Symptom

Creating a checkpoint failed:

```bash
sudo ./waypoint checkpoint "$SESSION" A
```

CRIU dump logs showed:

```text
Can't dump file ... (anon anon_inode:[pidfd])
Dump files failed
```

### Root Cause

The previous `bash_init` implementation started a long-lived child bash with Go
`os/exec` and kept `cmd.Process` alive. On this Go/Linux runtime, `os/exec` opens a pidfd
for the child process. That pidfd remained open in the checkpointed `bash_init`, and CRIU
could not dump it.

### Fix

The final implementation no longer keeps a long-lived child bash. Each `waypoint exec`
spawns a short-lived `/bin/bash -lc <payload>` while the fork lock is held, so there is no
idle child process or pidfd in the checkpoint image.

During earlier isolation, calling `cmd.Process.Release()` after starting the long-lived
bash also proved sufficient to remove the pidfd, but the long-lived child model had a
separate restore bug described below.

### Validation

Checkpoint creation now succeeds repeatedly:

```text
Checkpoint 'A' created successfully
```

## Fixed Bug 3: Marker Detection Could Complete on Terminal Echo

### Symptom

After the pidfd issue was fixed, post-checkpoint exec sometimes returned immediately with
an empty string and no side effect.

### Root Cause

The old PTY protocol appended a marker command containing the complete marker literal:

```bash
builtin printf '\n%s\n' "__CMD_DONE_<nonce>__"
```

Because PTY echo was enabled, the input line itself could contain the complete marker
before bash executed the user command. The server treated that echoed marker as command
completion, stripped the echo, and returned empty output.

### Fix Direction

During debugging, splitting the marker across `printf` arguments prevented the echo from
containing the complete marker. The final implementation removed the marker protocol
entirely by replacing the long-lived PTY shell with per-command `bash -lc` execution.

## Fixed Bug 4: Restored Long-Lived Bash Exited on First Input

### Symptom

After a successful checkpoint/restore or fork restore, the restored namespace had both
`bash_init` and bash alive:

```text
1 bash_init
11 bash
```

Process group and foreground TTY state also looked correct:

```text
bash pgrp=11 session=11 tty=<pts> tpgid=11
```

But the first post-restore exec killed bash before running the command:

```text
bash process exited before command completed
```

Side-effect probes showed that the command did not execute. Bash traps showed only the
`EXIT` trap firing, not `HUP`, `TERM`, `INT`, or `QUIT`.

### Experiments

The following did not fix the failure:

- Keeping the parent copy of the PTY slave open.
- Starting bash explicitly interactive with `-i`.
- Restoring with CRIU `--shell-job`.
- Leaving the PTY in canonical mode.
- Replacing the PTY with ordinary pipes while still checkpointing a long-lived child bash.

The pipe experiment was the important discriminator: the restored long-lived bash exited
on first input even without PTY state. That made the stable fix to avoid checkpointing an
idle child bash at all.

### Fix

`bash_init` is now a checkpointable namespace command server only. It accepts one
length-prefixed command over the Unix socket, serializes execution inside the live fork,
and runs:

```go
cmd := exec.Command("/bin/bash", "-lc", commandPayload)
cmd.Dir = "/"
cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
```

Output is captured from stdout/stderr and returned directly to the client. There is no
PTY, marker protocol, idle child bash, or checkpoint-time child pidfd.

This is also compatible with the MVP fork locking model:

- `waypoint exec` holds the per-fork file lock while the command runs.
- `waypoint snapshot` and `waypoint destroy` use the same per-fork lock.
- Therefore a checkpoint is taken while no per-command bash child is active.

### Tradeoff

Shell process state no longer persists across separate `waypoint exec` calls. Filesystem
state persists, and multi-line commands still work within a single exec payload, but
state such as `cd`, shell variables, aliases, and functions must be included in each
command if needed.

This tradeoff is intentional for the current greenfield MVP because the validated goal is
concurrent, isolated fork execution from immutable checkpoints. If persistent shell state
becomes a hard requirement, the next design should make it explicit instead of relying on
an implicitly checkpointed child bash.

## Final Validation Matrix

The final root-backed matrix passed:

- `go test ./...`
- static `bash_init` build
- `waypoint` build
- `sudo criu check`
- init shell with a minimal rootfs
- exec on `main` before checkpoint
- checkpoint `main` as `A`
- exec on restored `main`
- fork `A` into named forks `f1` and `f2`
- verify forks see checkpoint state but not post-checkpoint main mutations
- verify fork-private writes do not leak to main or sibling forks
- `fork --n 2`
- snapshot `f1` as checkpoint `B`
- fork `B` and verify recursive checkpoint state
- run commands concurrently on different forks
- verify same-fork commands serialize
- destroy a fork and verify later exec is rejected
- cleanup sessions and verify no leftover mounts

Key observed timings:

```text
CROSS_FORK_DURATION=2
SAME_FORK_DURATION=4
```

Two 2-second commands on different forks completed concurrently. Two 2-second commands on
the same fork serialized.

The final list output showed live forks on checkpoints `A` and `B`, with `main` running
from checkpoint `A`, and cleanup left no mounts under `/tmp/waypoint-sessions`.

## Definitive Root Causes (2026-07-06 investigation)

### Bug 4 was ARM64 PAC, not the shell architecture

Reproduced on Fedora 38 aarch64 (CRIU 3.18, CPU features include `paca pacg`, FPAC
behavior). strace of the restored long-lived bash during the first post-restore exec:

```text
read(0, "echo hello-post\n...", 1024) = 78
--- SIGILL {si_code=ILL_ILLOPN, si_addr=0xffff9ac5e1f8} ---
kill(11, SIGILL) = 0            # bash's own fatal-signal handler re-raising
+++ killed by SIGILL (core dumped) +++
```

Mechanism: Fedora builds bash/glibc with `-mbranch-protection` (`/bin/bash` contains
~5,600 `paciasp`/`autiasp` instructions). Return addresses live on the stack signed
with per-process PAC keys. CRIU 3.18 does not dump/restore the keys, so the restored
process gets fresh kernel-assigned keys, and the first function return that
authenticates a pre-dump signed LR faults. On FPAC cores that is an immediate SIGILL
at the `autiasp` instruction — confirmed by disassembly at the faulting address in a
minimal C reproducer:

- Static binary WITH PAC instructions: dies of SIGILL on first return after restore.
- Identical freestanding binary with ZERO PAC instructions: survives restore.
- Same PAC binary under CRIU v4.2 (has PAC key C/R): survives restore.

This explains every prior observation: pipes vs PTY made no difference (irrelevant to
PAC); no HUP/TERM/INT/QUIT traps fired (SIGILL is not trapped; bash's fatal handler
runs the EXIT trap then self-kills); the Go `bash_init` survived because Go emits no
PAC instructions; and the failure happened "on first input" because that is the first
function return after the restored `read(2)` completes.

### Bug 2 fix without losing the shell

The pidfd that broke CRIU dump is removed by calling `cmd.Process.Release()` right
after starting the long-lived bash (now in `cmd/bash-init/main.go`). The ephemeral
`bash -lc` redesign is not needed.

### Validated end state

With `Process.Release()` + CRIU v4.2 (`/usr/local/sbin/criu`), the long-lived PTY
shell passes: exec state setup (cwd `/root`, `MYVAR`, `sleep 600 &`), checkpoint of
the multi-process tree, post-restore state intact, `fork A` x2 inheriting the full
shell state (including the running background job), independent divergence, recursive
`snapshot f2 B` + `fork B` carrying the diverged state, concurrent cross-fork exec,
destroy, and mount-free cleanup. Fork restore latency ~300-420 ms (unoptimized).

Operational requirement: on aarch64 hosts with PAC, Waypoint requires CRIU >= 4.0.
A startup version/feature check in Waypoint would fail fast on 3.x hosts.

