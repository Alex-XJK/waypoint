# Changelog

## v0.7.0 — Parallel Forking
**Live forks from immutable checkpoints: run many divergent copies of one warm process tree concurrently**
- New public model: a session holds an immutable **checkpoint DAG** plus a registry of **live forks**. `checkpoint`/`snapshot` seal a live fork into a checkpoint; `fork` materializes any checkpoint into a new live fork — including multiple concurrent forks of the same checkpoint, each in its own PID/mount/net/IPC namespaces with a private OverlayFS upper layer.
- The `main` shell is now a first-class fork created by `init --shell`; recursive forking (fork → exec → snapshot → fork) is ordinary. The legacy destructive `restore` model has been removed.
- New commands: `fork <session> <checkpoint> [--id ID] [--n K]`, `snapshot <session> <fork> <checkpoint>` (with `--park` to persist a fork as a checkpoint without resuming it), `destroy <session> <fork>`, and `exec <session> <fork> -- <command>`.
- Exec protocol v2 ("WP2"): completion-FIFO-based command framing with real exit codes, plus bounded request headers (32 B), command payloads (1 MiB), and retained output (16 MiB) with explicit `request_too_large`/`output_limit` statuses.
- Persistent shell state — environment, cwd, and running background jobs — survives checkpoint, fork, and recursive snapshot. Same-fork operations serialize via file locks; different forks run fully concurrently.
- CRIU stabilization: PAC-aware CRIU ≥ 4.0 requirement on arm64 enforced by `setup check`, `--file-locks`, Node-friendly dump flags, pidfd release for long-lived children, PTY window-size restore, and per-fork restore logs. Fails fast when the sessions directory exceeds the Unix socket path limit.
- Optional performance instrumentation: CRIU images on tmpfs with async flush (`WAYPOINT_TMPFS_IMAGES`) and phase-level latency stats (`WAYPOINT_PHASE_STATS`). Baseline fork latency on a minimal rootfs is ~137 ms.
- `bash_init` is now statically linked and re-execs from inside the session overlay so CRIU can dump it reliably.
- Sessions start with a fixed, OCI-style environment (`PATH`, `HOME`, `TERM`, `LANG`) instead of inheriting the invoking user's shell environment (which was baked into every checkpoint); `WAYPOINT_*` plumbing vars no longer reach the guest shell.
- Removed ldd-based "library healing" of rootfs binaries; a rootfs must ship its own libraries. Shell startup failures now surface the shell's own error output (e.g. the loader naming a missing library) in the `init` error, and `bash_init` is verified static at session start.
- Removed host `gpgv`/Ubuntu-keyring staging from `build` — a relic of the pre-network-namespace era; sessions are loopback-only, so in-session `apt` cannot fetch packages, and Dockerfile `RUN apt-get` steps run under buildah's own networking. Images must ship their own apt tooling.
- `init` and `build` now share one rootfs model: the source is snapshotted into the session (`cp --reflink=auto` — instant on xfs/btrfs, plain copy elsewhere) and used as the overlay lower, so editing the source dir during a session can no longer corrupt the overlay; name-resolution seeding (`/etc/hosts`, `nsswitch.conf`, resolv.conf, if the image ships none) now applies to both paths, through the merged view.
- Sessions get their own UTS namespace with hostname `waypoint` — guests no longer report (or, as root, could rename) the host's hostname; the hostname survives CRIU restore in forks.
- Sandbox device nodes are now created solely at session start by `bash_init` (umask-proof, works for images with an empty `/dev`); the build-time duplicate is gone, and `bash_init` refuses to run outside waypoint-provided namespaces. Session paths are validated against OverlayFS option separators (`:`/`,`).
- Session, checkpoint, and fork IDs are validated before they are used to build a path or a mount option: they must start with a letter or digit and may otherwise contain only letters, digits, `.`, `_` and `-`. This closes a path traversal (a checkpoint ID of `../../../../tmp/x` wrote CRIU images outside the session tree, where `cleanup` could never reclaim them) and stops an ID containing `:` or `,` from corrupting the overlay `lowerdir` list. Validation now happens before the dump, so a rejected ID no longer costs a live fork or leaves an unforkable checkpoint behind.
- `list`, `cleanup`, and `suspend` now reject unknown flags instead of silently ignoring them — a mistyped `--force` used to fall through to the interactive cleanup path.
- New `suspend <session>` command: ends all live forks (a cousin of `cleanup` that keeps every checkpoint on disk and the session registered), sweeps leftover processes and mounts, and flushes tmpfs-resident CRIU images to durable disk; `fork` any checkpoint later to resume. Un-snapshotted fork divergence is discarded — checkpoints are the durable state.
- The session registry location is now configurable (`session_info_dir` / `WAYPOINT_SESSION_INFO_DIR`, same precedence as the other settings); with it and `sessions_dir` on durable storage, suspended sessions survive reboots.
- Fixed preserve-mode cleanup deleting not-yet-flushed tmpfs images: paths that keep disk state (suspend, `preserve_session_on_cleanup`) now flush pending checkpoint images to disk first and leave tmpfs copies in place if a flush fails.
- Testing and docs: `scripts/demo.sh` is an asserting end-to-end test of the full CLI surface; new `docs/architecture.md`, `docs/exec-protocol.md`, and `AGENTS.md`.

## v0.6.2 — CRIU Compatibility and Restore Readiness
**Synchronous restore completion and broader workload support**
- Made CRIU restore synchronous so `restore` returns only after the restored process is ready for immediate follow-up commands.
- Added CRIU support for checkpointing and restoring processes that hold file locks.
- Added inode reverse-map and deleted-file remapping support for Node.js workloads with inotify watches and unlinked-but-open files.
- Improved cgroup handling and restore preparation to reduce process-ID reuse conflicts.

## v0.6.1 — Setup Workflow and CLI Inspection
**Installation polish, release automation, and small usability fixes**
- Added `setup` script for building, installing, checking, uninstalling, and cleaning Waypoint.
- Added an Ubuntu/Debian dependency helper and host check workflow for CRIU, OverlayFS, and related system utilities.
- Extended `list` to show all recorded sessions when no session ID is provided.
- Added `info` to inspect system configuration, host dependencies, sessions, and checkpoint metadata as JSON.
- Fixed Dockerfile/buildah image builds for context directory names that need image-tag sanitization.

## v0.6.0 — Waypoint Rename
**Project rename and public identity update**
- Renamed the project from **Checkpoint-lite** to **Waypoint**. Older blog posts, videos, and public announcements may still use the Checkpoint-lite name.

## v0.5.2 — Robust Shell & Cleanup Refinements
**Reliability-focused polish for shell sessions, restore preparation, and cleanup**
- Improved shell-session command handling for longer and multi-line commands.
- Made shell command cancellation and timeout handling more predictable for disconnected clients.
- Improved process cleanup before restore to reduce CRIU restore conflicts.
- Added runtime filesystem and device setup improvements for Dockerfile-built environments.
- Added `preserve_session_on_cleanup` for users who want cleanup to unmount and stop processes while keeping session files for debugging.

## v0.5.0 — Isolated Shell Sessions & Dockerfile-Based Environment Build
**Persistent shell isolation and environment construction workflow**
- Designed and implemented an RPC-based Shell Session Manager to maintain persistent, isolated shell states across checkpoints and restores.
- Extended the `exec` command to execute within the context of a managed shell session.
- Introduced a new `build` command to construct environments from a Dockerfile.
- Updated the CLI to support shell-session lifecycle management and Dockerfile-based build workflows.
- Refactored project structure.

## v0.4.0 — New Filesystem Architecture
**Major redesign of filesystem checkpointing and restore pipeline**
- Introduced a fully new filesystem architecture with parent-tracking logic.
- Re-implemented `init`, `create`, and `restore` to match the new model.
- Verified all functionalities through manual tests.
- Added various minor fixes and debugging improvements.

## v0.3.0 — Sandbox Mode & Customizable Path Config
**Filesystem isolation + configuration overhaul**
- Added Sandbox Mode using Linux namespaces and `pivot_root`.
- Optimized checkpoint/restore by excluding the working directory.
- Added customizable path configuration with a 5-level precedence order.

## v0.2.0 — Performance & Usability Improvements
**Incremental improvements before the major FS redesign**
- Added quiet CSV output mode.
- Supported skipping memory checkpoints.
- Improved default cleanup behavior and cleanup levels.
- Updated installation instructions, documentation, and workflow diagram (v0.2.1).
- Added multithreaded reader-writer test.

## v0.1.0 — Foundational Features & Experimental Work
**Initial system architecture, CLI, and experimental features**
- Designed core structures and initial CLI.
- Refactored core checkpoint/restore logic.
- Fixed CRIU TTY and background-process issues.
- Added experimental features:
    - Parallel-Checkpoints (validated)
    - Unsafe-FsRestore (disabled)
- Added a test target program.
- Improved README and added Go installation guide.

## v0.0.1 — Initial Setup
**Project bootstrap**
- Initial commit and technical architecture selection.
- Set up the Go environment and drafted core structures.
- Added CLI usage documentation.
