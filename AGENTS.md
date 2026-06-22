# Agent Notes

This repository is Linux/CRIU/OverlayFS heavy. Many meaningful checks require root on a
Linux host. Do not assume macOS static builds prove runtime behavior.

## Current Parallel-Forking Context

Read these first when working on parallel forking:

1. `parallel-forking-design.md` - north-star architecture and goals.
2. `milestone-1-plan.md` - original de-risking plan.
3. `milestone-1-handoff.md` - historical handoff from the macOS implementation pass.
4. `milestone-2-plan.md` - current buildout plan after Linux validation.

Milestone 1 has been runtime-validated on Linux after fixes. The important result is that
the CRIU materializer can create multiple concurrent forks from one checkpoint with
isolated filesystem mutations.

## Validated Commands

On a suitable Linux host:

```bash
go test ./...
go build -o waypoint ./cmd/waypoint
go build -o bash_init ./cmd/bash-init
sudo criu check
sudo ./waypoint init /path/to/rootfs --shell
sudo ./waypoint exec <session> 'echo ok'
sudo ./waypoint create <session> ckpt1
sudo ./waypoint fork <session> ckpt1
sudo ./waypoint fork-exec <session> <fork-id> 'cat /tmp/some-file'
sudo ./waypoint cleanup <session> --force
```

For tests, a minimal rootfs can work if it contains `/bin/bash`, basic shared libraries,
`/lib64/ld-linux-x86-64.so.2`, and any commands used in the test such as `/bin/cat`.

## Non-Obvious Implementation Constraints

- `bash_init` must pivot into the session overlay root before checkpointing.
- `bash_init` must re-exec from inside the overlay (`/.waypoint/bash_init`) so CRIU can
  resolve its executable mapping.
- Do not keep host-root log files open in checkpointed processes; they break CRIU fd dump.
- Host access to restored Unix sockets should use `/proc/<pid>/root/<canonical-socket>`.
- Do not bind-mount the session temp dir into the namespace for the socket; CRIU 4.2
  failed on that mount shape.
- CRIU 4.2 dump-side external mount syntax for mounts is mountpoint-based:
  `--external mnt[/]:waypoint-work`, not `mnt[<mount-id>]:waypoint-work`.
- Restore should be synchronous and should use `--restore-detached` plus `--pidfile`.
- Namespace-local task IDs in CRIU images are not host PIDs. If the image contains PID 1,
  cleanup should target the namespace init host PID, not `/proc/1`.

## Architectural Rules

- Fork paths must not mutate session-global `current` state.
- Treat session-global `current` as legacy compatibility or as a future `main` fork.
- Shared checkpoint layers are immutable. Live forks own private upper/work dirs.
- Commands on different forks should eventually run concurrently.
- Commands on the same fork should serialize.
- Snapshot/destroy must not race with active exec on the same fork.
- DeltaBox-style frozen fork templates are a later optimization behind the same
  `Materializer` interface. Do not redesign the core API around that fast path yet.

## Before Ending Work

- Run `gofmt` on edited Go files.
- Run `go test ./...` when Go code changes.
- Build both binaries when touching runtime/session code:

```bash
go build -o waypoint ./cmd/waypoint
go build -o bash_init ./cmd/bash-init
```

- If you create root-owned sessions during testing, clean them:

```bash
sudo ./waypoint cleanup <session> --force
```

- Check for leftover mounts under `/tmp/waypoint-sessions/<session>` with `findmnt`.

