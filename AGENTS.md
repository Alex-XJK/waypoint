# Agent Notes

This repository is Linux/CRIU/OverlayFS heavy. Many meaningful checks require root on a
Linux host. Do not assume macOS static builds prove runtime behavior.

## Current Parallel-Forking Context

Read these first when working on parallel forking:

1. `parallel-forking-design.md` - north-star architecture and goals.
2. `parallel-forking-mvp-plan.md` - current greenfield MVP implementation plan.
3. `parallel-forking-runtime-fix-plan.md` - Linux runtime validation notes and
   `bash_init` startup fixes.
4. `milestone-1-plan.md` - original de-risking plan.
5. `milestone-1-handoff.md` - historical handoff from the macOS implementation pass.

Milestone 1 has been runtime-validated on Linux after fixes. The important result is that
the CRIU materializer can create multiple concurrent forks from one checkpoint with
isolated filesystem mutations.

The current MVP direction is greenfield. Compatibility with the old session-global
`current`/`create`/`restore` flow is not important unless the user explicitly asks for it.

## MVP Architecture Rules

- Public model:
  - Session = checkpoint store + live fork registry.
  - Checkpoint = immutable DAG node.
  - Fork = live mutable instance of one checkpoint.
  - Snapshot = live fork -> new checkpoint.
  - Fork = checkpoint -> live fork.
- Treat `main` as just another fork created by `init --shell`.
- Fork paths must not mutate session-global `current` state.
- Shared checkpoint layers are immutable. Live forks own private upper/work dirs.
- Recursive forking should be ordinary: fork a checkpoint, exec, snapshot the fork, then
  fork the new checkpoint.
- Checkpoint metadata should use `ParentID` for the logical DAG edge and `LayerIDs`
  for the resolved filesystem chain from oldest to newest.
- OverlayFS lowerdirs are built by reversing `LayerIDs`, then appending the original
  rootfs as the lowest-priority lowerdir.
- Commands on different forks should run concurrently.
- Commands on the same fork must serialize.
- Snapshot/destroy must not race with active exec on the same fork.
- Use file locks, not process-local mutexes, because CLI commands run in separate
  processes.
- Do not hold the session lock while running long CRIU, shell, mount, or process-kill
  operations.
- DeltaBox-style frozen fork templates are a later optimization behind the same
  `Materializer` interface. Do not redesign the core API around that fast path yet.

## Target MVP CLI

Preferred interface:

```bash
waypoint init <rootfs> --shell [--quiet]
waypoint checkpoint <session> <checkpoint-id>
waypoint fork <session> <checkpoint-id> [--id <fork-id>] [--n K] [--lazy-pages]
waypoint exec <session> <fork-id> -- <command>
waypoint snapshot <session> <fork-id> <checkpoint-id>
waypoint destroy <session> <fork-id>
waypoint list <session> [--json]
waypoint cleanup <session> [--force]
```

Old commands such as `create`, `restore`, and `fork-exec` can be removed or implemented
as aliases if that is convenient. Do not let them distort the new model.

## Non-Obvious Runtime Constraints

- On aarch64 hosts with pointer authentication (`paca`/`pacg` in `/proc/cpuinfo`),
  CRIU >= 4.0 is required: older CRIU does not checkpoint PAC keys, so restored
  PAC-built binaries (bash, glibc userland) die with SIGILL at the first
  authenticated return. `EnsureCriuCompatible()` enforces this at runtime. See the
  "Definitive Root Causes" section of `parallel-fork-runtime-bugs.md`.
- Do not keep an os/exec pidfd open for a long-lived child in a checkpointed
  process; call `cmd.Process.Release()` after start (CRIU cannot dump pidfds).
- `bash_init` must pivot into the session overlay root before checkpointing.
- `bash_init` must re-exec from inside the overlay (`/.waypoint/bash_init`) so CRIU can
  resolve its executable mapping.
- Prefer building `bash_init` statically for runtime validation:
  `env CGO_ENABLED=0 GOCACHE=/tmp/waypoint-go-cache go build -o bash_init ./cmd/bash-init`.
- If `bash_init` is dynamic, its loader and shared libraries must exist inside the rootfs.
- Do not keep host-root log files open in checkpointed processes; they break CRIU fd dump.
- Host access to restored Unix sockets should use `/proc/<pid>/root/<canonical-socket>`.
- Do not bind-mount the session temp dir into the namespace for the socket; CRIU 4.2
  failed on that mount shape.
- CRIU 4.2 dump-side external mount syntax for mounts is mountpoint-based:
  `--external mnt[/]:waypoint-work`, not `mnt[<mount-id>]:waypoint-work`.
- Restore should be synchronous and should use `--restore-detached` plus `--pidfile`.
- Namespace-local task IDs in CRIU images are not host PIDs. If the image contains PID 1,
  cleanup should target the namespace init host PID, not `/proc/1`.

## Validated Commands

Use a writable Go cache in this environment:

```bash
env GOCACHE=/tmp/waypoint-go-cache go test ./...
env CGO_ENABLED=0 GOCACHE=/tmp/waypoint-go-cache go build -o bash_init ./cmd/bash-init
env GOCACHE=/tmp/waypoint-go-cache go build -o waypoint ./cmd/waypoint
sudo criu check
```

On a suitable Linux host, the runtime prototype has been validated with:

```bash
sudo env WAYPOINT_BASH_INIT_SRC="$PWD/bash_init" ./waypoint init /path/to/rootfs --shell
sudo ./waypoint exec <session> 'echo ok'
sudo ./waypoint create <session> ckpt1
sudo ./waypoint fork <session> ckpt1
sudo ./waypoint fork-exec <session> <fork-id> 'cat /tmp/some-file'
sudo ./waypoint fork <session> ckpt1 --n 2
sudo ./waypoint cleanup <session> --force
```

For tests, a minimal rootfs can work if it contains `/bin/bash`, basic shared libraries,
`/lib64/ld-linux-x86-64.so.2`, `/tmp`, `/proc`, `/sys`, `/dev/null`, and any commands used
in the test such as `/bin/cat`, `/bin/ls`, or `/usr/bin/readlink`.

## Before Ending Work

- Run `gofmt` on edited Go files.
- Run `env GOCACHE=/tmp/waypoint-go-cache go test ./...` when Go code changes.
- Build both binaries when touching runtime/session code:

```bash
env GOCACHE=/tmp/waypoint-go-cache go build -o waypoint ./cmd/waypoint
env CGO_ENABLED=0 GOCACHE=/tmp/waypoint-go-cache go build -o bash_init ./cmd/bash-init
```

- If you create root-owned sessions during testing, clean them:

```bash
sudo ./waypoint cleanup <session> --force
```

- Check for leftover mounts under the session directory with `findmnt`, for example:

```bash
sudo findmnt -R /tmp/waypoint-sessions/<session>
sudo findmnt -R /mydata/waypoint-sessions/<session>
```
