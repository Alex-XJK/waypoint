# Agent Notes

This repository is Linux/CRIU/OverlayFS heavy. Most meaningful checks require **root on a
Linux host**. Builds prove nothing about runtime behavior — a green `go build` on any
machine says nothing about whether CRIU can dump or restore a fork.

## What Waypoint Is

Waypoint checkpoints and forks **live Linux environments**: a running process tree —
e.g. a bash shell with its variables, cwd, and background jobs — together with its
filesystem. A session starts from a rootfs and a live shell (the `main` fork). At any
point that live state can be sealed into an immutable checkpoint, and any checkpoint
can be forked into many concurrent, fully divergent live copies of that exact moment,
warm in-memory state included. The representative workload is agent-style search:
fork one warm environment once per candidate action, evaluate the actions in
parallel, keep the winner, snapshot it as a new checkpoint, recurse — with cheap
backtracking to any earlier node.

Two mechanisms make this work:

- **CRIU** dumps and restores the process tree (memory, fds, PTY, namespaces), so a
  fork resumes mid-execution — background processes keep running from where the
  checkpoint froze them.
- **OverlayFS** seals each checkpoint's filesystem delta as an immutable, shareable
  layer and gives every live fork a private copy-on-write upper, so forking never
  copies the rootfs and checkpoint history forms a cheap layer chain.

Checkpoints form a DAG (fork → exec → snapshot → fork again is the normal loop), and
each live fork runs isolated in its own PID/mount/net/IPC/UTS namespaces. On a minimal
rootfs a fork materializes in roughly 140 ms and a checkpoint/snapshot takes roughly
240 ms — cheap enough to fork per candidate action.

## Orientation

Read in this order:

1. `docs/architecture.md` — the canonical architecture guide: mental model, component
   map, and how a fork is actually materialized. **Start here.**
2. `docs/exec-protocol.md` — the v2 exec wire protocol (framing, exit codes, statuses).
3. `docs/INSTALL.md` — host setup and the `setup`/`make` entry points.

## Architecture Rules

- Public model:
  - Session = checkpoint store + live fork registry.
  - Checkpoint = immutable DAG node.
  - Fork = live mutable instance of one checkpoint.
  - Snapshot = live fork -> new checkpoint (`--park` persists without resuming the fork).
  - Fork = checkpoint -> live fork. Multiple concurrent forks of one checkpoint are
    ordinary, each in its own PID/mount/net/IPC/UTS namespaces with a private overlay upper.
- `main` is just another fork, created by `init --shell`. There is no destructive
  restore; state is always materialized as checkpoint -> new fork.
- Fork paths must not mutate session-global `current` state.
- Shared checkpoint layers are immutable. Live forks own private upper/work dirs.
- Recursive forking is ordinary: fork a checkpoint, exec, snapshot the fork, then fork
  the new checkpoint. No special-casing of where a checkpoint came from.
- Checkpoint metadata uses `ParentID` for the logical DAG edge and `LayerIDs` for the
  resolved filesystem chain, oldest to newest.
- OverlayFS lowerdirs are built by reversing `LayerIDs`, then appending the original
  rootfs as the lowest-priority lowerdir.
- Commands on different forks run concurrently; commands on the same fork must serialize.
- Snapshot/destroy must not race with an active exec on the same fork.
- Use **file locks**, not process-local mutexes — each CLI command is a separate process.
- Never hold the session lock while running long CRIU, shell, mount, or process-kill
  operations.

## CLI

As implemented in `cmd/waypoint/main.go`:

```bash
waypoint init <work-directory> [--quiet] [--shell]
waypoint build <dockerfile-directory> [--quiet]     # buildah-based rootfs build
waypoint checkpoint <session> <checkpoint-id>       # alias: create
waypoint fork <session> <checkpoint-id> [--id ID] [--n K]
waypoint exec <session> <fork-id> -- <command>
waypoint snapshot <session> <fork-id> <checkpoint-id> [--park]
waypoint destroy <session> <fork-id>
waypoint list [session] [--json]
waypoint info [session [checkpoint-id]]
waypoint cleanup <session> [--force]
waypoint version
```

Notes:
- `create` is a plain alias of `checkpoint`.
- `fork-exec <session> <fork-id> <cmd> [args...]` calls the same `ExecuteForkCommand`
  as `exec`; prefer `exec`.
- `__waypoint_restore_fork_child` is an internal re-exec entry point, not a user command.
- Note the `--` separator in `exec`; it is required.
- Opt-in env toggles: `WAYPOINT_TMPFS_IMAGES` (CRIU images on tmpfs with async flush)
  and `WAYPOINT_PHASE_STATS` (phase-level latency instrumentation). Both default off.

## Build, Install, Check

The repo has a `setup` shell script with a thin `Makefile` wrapper. Prefer these:

```bash
./setup build      # -> bin/waypoint, bin/bash_init   (or: make build)
./setup test       # -> go test ./...                  (see "Testing" below)
sudo ./setup check # host commands + kernel + CRIU version/features
sudo ./setup install
./setup clean
```

`setup install` places `waypoint` in `$PREFIX/bin`, `bash_init` in
`$PREFIX/libexec/waypoint`, and writes `/etc/waypoint/config.json` pointing
`bash_init_src` at the installed helper. Override with `PREFIX`, `BINDIR`,
`LIBEXECDIR`, `CONFIG_PATH`; `FORCE_CONFIG=1` replaces an existing config.

Without an install, `bash_init_src` defaults to `./bash_init`, so for local runtime work
set it explicitly:

```bash
sudo -E env WAYPOINT_BASH_INIT_SRC="$PWD/bin/bash_init" ./bin/waypoint init ...
```

`./setup build` builds `bash_init` with `CGO_ENABLED=0`, so it comes out **statically
linked**. Keep it that way: `bash_init` is staged into the session rootfs and re-execs
there as PID 1, and a dynamic build only works if its ELF loader and shared libraries are
also staged in. `StartShell` does stage them, but a static binary removes the failure mode
entirely. If you build `bash_init` by hand for runtime work, do the same:

```bash
env CGO_ENABLED=0 go build -o bin/bash_init ./cmd/bash-init
```

## Testing

Unit tests are narrow — they cover pure helper logic only. `go test ./...` passing is
necessary but nowhere near sufficient: none of the CRIU/mount/namespace paths are
exercised. Do not report "tests pass" as validation of runtime behavior.

Real validation is the root runtime path. `scripts/demo.sh` is an asserting end-to-end
test of the full CLI surface (init → exec → checkpoint → fork → divergence → snapshot /
`--park` → destroy → list/info → cleanup); it exits non-zero on any failed assertion:

```bash
sudo ./scripts/demo.sh
```

A minimal test rootfs needs `/bin/bash`, its shared libraries, the ELF loader
(`/lib64/ld-linux-x86-64.so.2` on x86-64, `/lib/ld-linux-aarch64.so.1` on arm64), plus
`/tmp`, `/proc`, `/sys`, and any commands the test uses (`/dev` is assembled at
session start by `bash_init`). The guest shell runs with a fixed environment
(`PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`, `HOME=/root`,
`TERM`, `LANG`) — never the invoking user's — so put commands under one of those
standard PATH directories.

## Non-Obvious Runtime Constraints

- On aarch64 hosts with pointer authentication (`paca`/`pacg` in `/proc/cpuinfo`),
  **CRIU >= 4.0 is required**: older CRIU does not checkpoint PAC keys, so restored
  PAC-built binaries (bash, glibc userland) die with SIGILL at the first authenticated
  return. This is enforced by `./setup check` (`CRIU_MIN_VERSION=4.0`), **not at
  runtime**.
- Do not keep an `os/exec` pidfd open for a long-lived child in a checkpointed process;
  call `cmd.Process.Release()` after start (CRIU cannot dump pidfds).
- `bash_init` must pivot into the session overlay root before checkpointing.
- `bash_init` must re-exec from inside the overlay (`/.waypoint/bash_init`) so CRIU can
  resolve its executable mapping.
- After `pivot_root`, mount `/proc` and `/sys` relative to the **new** root, not the old
  host `chrootDir`.
- If `bash_init` is dynamic, its loader and shared libraries must exist inside the rootfs.
- Do not keep host-root log files open in checkpointed processes; they break CRIU's fd
  dump. `bash_init` detaches stdio to namespace `/dev/null` once the shell is ready.
- Host access to restored Unix sockets should use `/proc/<pid>/root/<canonical-socket>`.
- Do not bind-mount the session temp dir into the namespace for the socket; CRIU 4.2
  fails on that mount shape.
- CRIU 4.2 dump-side external mount syntax is mountpoint-based:
  `--external mnt[/]:waypoint-work`, not `mnt[<mount-id>]:waypoint-work`.
- Restore should be synchronous and should use `--restore-detached` plus `--pidfile`.
- Namespace-local task IDs in CRIU images are not host PIDs. If the image contains PID 1,
  cleanup must target the namespace init host PID, not `/proc/1`.
- Startup is readiness-gated: `StartShell` waits for the control socket to be dialable
  and detects early exit. Never report success on a PID alone.

## Before Ending Work

- Run `gofmt` on edited Go files.
- Rebuild both binaries when touching runtime/session code: `./setup build` (plus a
  static `bash_init` if you are about to do runtime validation).
- For any change to CRIU, mounts, namespaces, `bash_init`, or the exec protocol, run a
  **real root runtime check** — `sudo ./scripts/demo.sh` at minimum. Builds and
  `go test` do not exercise any of this.
- Clean up root-owned sessions created during testing:

```bash
sudo ./waypoint cleanup <session> --force
```

- Check for leftover mounts under the session directory:

```bash
sudo findmnt -R /tmp/waypoint-sessions/<session>
```
