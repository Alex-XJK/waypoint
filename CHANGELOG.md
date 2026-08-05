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
