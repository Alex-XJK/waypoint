# Working on Waypoint

This file provides general repository orientation and development guidance. Keep it
focused on the current design and reusable practices; put task plans, investigation
logs, benchmark results, and release-specific findings elsewhere.

## Collaborating with the human developer

Understand the affected code before changing it. Prefer extending the existing
components and abstractions, and keep changes focused enough for the developer to
review and explain. Preserve the session/checkpoint/fork model and the boundaries
between the CLI, runtime library, and shell supervisor.

For implementation changes, give the developer a brief high-level design explanation:

- Which components are involved and what responsibility each has.
- How the change fits into the existing control flow or state lifecycle.
- Which architectural invariants it preserves, and any meaningful tradeoffs or
  compatibility implications.
- What was validated and what remains unverified.

Scale this explanation to the change; a few sentences are enough for a small fix.
A list of edited files alone does not explain the design. Surface substantial
architectural departures early, with their motivation and alternatives, rather than
introducing a second state model or execution mechanism incidentally. For changes
limited to documentation or tests, summarize the content or coverage and validation;
a separate architecture explanation is unnecessary.

## Repository orientation

Waypoint is a Linux tool for checkpointing and concurrently forking live process
state together with filesystem state. It combines CRIU process-tree dumps/restores,
OverlayFS copy-on-write layers, and a persistent Bash shell supervised on a PTY.
The runtime requires root and suitable kernel features. It shares host networking;
it does not provide a complete container security boundary.

Start with [docs/architecture.md](docs/architecture.md) for the overall design,
[docs/exec-protocol.md](docs/exec-protocol.md) for shell communication, and
[docs/INSTALL.md](docs/INSTALL.md) for host setup. [README.md](README.md) documents
user-facing usage. Verify details against the implementation and tests when these
sources disagree, especially namespace and platform assumptions.

| Location | Responsibility |
|---|---|
| `cmd/waypoint/` | CLI parsing, presentation, exit status, and internal re-exec entry points. |
| `pkg/waypoint/manager.go`, `config.go` | Session registry, paths, configuration, cross-process locks, and atomic record writes. |
| `pkg/waypoint/checkpoint.go`, `fork.go` | Checkpoint DAG, live fork lifecycle, materialization, snapshotting, and exec client. |
| `pkg/waypoint/build.go`, `overlay.go`, `criu.go` | Rootfs staging/Buildah builds, shell startup, OverlayFS, and CRIU orchestration. |
| `pkg/waypoint/copy.go`, `inspect.go`, `cleanup.go` | Fork-aware file transfer, runtime inspection, and process/mount teardown. |
| `pkg/waypoint/imagestore.go`, `criustats.go` | Optional tmpfs image storage/flush and phase timing. |
| `cmd/bash-init/` | Namespace init and persistent shell supervisor; exec protocol server. |
| `contrib/bash-completion/`, `scripts/` | Bash completion and executable validation workflows. |
| `setup`, `Makefile`, `.github/workflows/` | Build/install/check entry points and CI/release automation. |

## Architectural invariants

- A **session** owns a checkpoint store and a live fork registry. A **checkpoint**
  stores a sealed filesystem delta and process image. A **fork** is a live mutable
  instance, with its own writable overlay and process tree. `main` is the initial
  fork, started by `init --shell` or `build`; plain `init` does not start a shell.
- Checkpoints form a DAG. `ParentID` is the logical parent; `LayerIDs` is the
  resolved filesystem chain from oldest to newest. Overlay lowerdirs reverse that
  chain and put the original rootfs last, at lowest priority.
- Sealed filesystem layers and checkpointed process state are shared inputs to
  materialization. Stage input rootfs directories into session-owned `original/`;
  do not use a mutable caller directory as a shared lower layer. Keep fork writes
  in private upper/work directories. Preserve OverlayFS whiteout semantics and
  recursive snapshot/fork behavior.
- Snapshotting normally seals a fork and rebases/resumes it on the new checkpoint.
  Parking saves the checkpoint without resuming that fork. Backtracking materializes
  a new fork from an earlier checkpoint; there is no destructive `restore` command
  or session-global mutable "current checkpoint" to switch.
- The CLI runs as separate processes. Use the existing file locks for coordination;
  a process-local mutex cannot protect another invocation. Same-fork exec, copy,
  snapshot, and destroy operations serialize; different forks can run concurrently.
  Keep session locks around short metadata transitions, outside long CRIU, shell,
  mount, and teardown work. Keep lockfiles in the stable session `locks/` directory,
  outside removable fork directories. Preserve existing lock ordering and atomic
  writes.
- Runtime views belong to namespaces. The host's session `work/` directory is not
  a universal live view of every fork. Resolve live sockets/files through the
  verified fork process, using the existing `/proc/<pid>/root` helpers. Preserve
  PID/start-time identity checks, path validation, and scoped teardown.
- Keep CLI parsing and presentation in `cmd/waypoint`, lifecycle/storage logic in
  `pkg/waypoint`, and PTY/shell supervision in `cmd/bash-init`. Reuse these paths
  when adding operations so locking, configuration, and failure handling agree.

## CLI conventions

The command dispatch in `cmd/waypoint/main.go` and copy parser in
`cmd/waypoint/copy.go` define the supported syntax. Common forms are:

```text
waypoint init <rootfs-directory> [--quiet] [--shell]
waypoint build <dockerfile-directory> [--quiet]
waypoint checkpoint <session> <checkpoint-id>
waypoint fork <session> <checkpoint-id> [--id ID] [--n K]
waypoint exec <session> <fork-id> -- <command>
waypoint cp <session> <host-path> <fork-id>:/<path>
waypoint cp <session> <fork-id>:/<path> <host-path>
waypoint snapshot <session> <fork-id> <checkpoint-id> [--park]
waypoint destroy <session> <fork-id>
waypoint list
waypoint list <session> [--json]
waypoint info [session [checkpoint-id]]
waypoint suspend <session>
waypoint cleanup <session> [--force]
waypoint version
```

`checkpoint` snapshots `main`; `snapshot` selects a fork. `main` cannot be parked.
`destroy` removes a selected fork; session teardown uses `suspend` or `cleanup`.
`suspend` ends the session's live forks while retaining checkpoint history; it does
not snapshot unsaved fork edits. `create` and `fork-exec` are legacy aliases; prefer
the primary commands. Names beginning `__waypoint_` are internal re-exec entry points.

`exec` requires `--` followed by exactly one argument: the Bash input string,
delivered to the fork's shell unchanged. Extra arguments are refused, not joined;
unparseable input is refused before the fork lock is taken. Preserve the PTY
protocol's completion signaling and command exit status; stdout/stderr share the
PTY. `cp` requires exactly one fork endpoint and uses exact destination paths. It
supports regular files/directories, with explicit limits on symlinks and metadata
preservation documented in `pkg/waypoint/copy.go`.

Changes to public commands should account for help/usage, documentation, completion,
and their tests. Completion obtains dynamic IDs through existing read-only CLI
commands and filters their output; keep completion free of runtime mutations.

## Build and configuration

Use the Go version declared in `go.mod`. The Makefile delegates to `setup`:

```bash
./setup build       # bin/waypoint and bin/bash_init; also: make build
./setup test        # Bash completion tests and go test ./...; also: make test
sudo ./setup check # host commands, kernel features, and CRIU checks
```

`bash_init` must be statically linked: it re-execs inside arbitrary session rootfses,
and startup rejects a dynamically linked helper. `setup build` sets `CGO_ENABLED=0`
for it. Rebuild both binaries when changing runtime code or the exec protocol so
local validation uses a matching pair. For local runtime work, select the helper
explicitly, for example:

```bash
sudo env WAYPOINT_BASH_INIT_SRC="$PWD/bin/bash_init" \
  ./bin/waypoint init /path/to/rootfs --shell
```

Configuration is resolved in `pkg/waypoint/config.go`: direct `WAYPOINT_*` overrides
win over a selected config file, then defaults apply. `WAYPOINT_CONFIG` selects an
explicit file; otherwise discovery checks beside the executable, user configuration,
then system configuration. Use the public loading paths when accessing persisted
sessions so configuration is resolved consistently.

`WAYPOINT_SESSIONS_DIR` holds runtime/checkpoint data; `WAYPOINT_SESSION_INFO_DIR`
holds registry records. Both default under `/tmp`. Use durable locations for both
when persistence across reboots matters. Keep runtime paths short enough for Unix
sockets and valid for OverlayFS mount options. `WAYPOINT_TMPFS_IMAGES` and
`WAYPOINT_PHASE_STATS` are opt-in; pending tmpfs images are not durable until flushed.

Installation and dependency setup modify the host. `sudo ./setup install` installs
the CLI, static helper, Bash completion, and a helper-path config, preserving existing
configuration by default. See `./setup help` and `docs/INSTALL.md` for path overrides
and dependency installation; neither is needed for a documentation edit.

## Runtime constraints and validation

Preserve the startup sequence: enter the managed mount/PID/IPC/UTS environment,
make mount propagation private, pivot into the overlay, assemble guest `/dev`,
`/proc`, and `/sys`, and re-exec the helper inside the rootfs. Readiness requires a working shell control socket, not
just a PID. Avoid retaining host-root log descriptors or child pidfds in checkpointed
processes; keep the existing descriptor-release and stdio-detachment handling.

A supplied rootfs needs Bash, its ELF loader/shared libraries, and the commands the
workload uses. Guest configuration starts from controlled defaults and, for builds,
OCI image `ENV`/`WORKDIR`; unattended shell settings and internal plumbing take
precedence. The invoking host's environment is not passed through wholesale.

CRIU compatibility depends on the host. `setup check` verifies features and enforces
a CRIU minimum on aarch64 for pointer-authentication support. Preserve the existing
external-mount mapping and synchronous detached-restore protocol when changing CRIU
integration; a successful build cannot establish that dump/restore works.

Choose validation for the affected behavior:

- Go tests cover configuration, metadata, filesystem helpers, locks, parsing, and
  related logic. `./setup test` also runs the Bash completion tests. These do not
  replace real CRIU/namespace lifecycle checks.
- For changes to CRIU, mounts, namespaces, lifecycle, `bash_init`, or the exec
  protocol, run `sudo ./scripts/demo.sh` as the baseline runtime check, plus focused
  coverage for the change. It rebuilds binaries and asserts CLI behavior and cleanup;
  its Dockerfile section requires Buildah and reports a skip if unavailable.
- `sudo ./scripts/test-search-workflow.sh` adds an offline Dockerfile build, file
  transfer, concurrent branching, recursive snapshots/backtracking, export, and
  cleanup workload. Its header documents prerequisites and scaling options.
- Run Waypoint runtime operations as root through `sudo` or a root test harness.
  Use private test sessions/paths, clean only resources created by the test, and
  verify that their processes and mounts are gone. Use the same registry/config
  overrides for cleanup as for creation.
- Run `gofmt` on changed Go files and check the diff. For documentation-only edits,
  verify statements, examples, and links; runtime tests are unnecessary. For test
  changes, exercise the affected tests. Report commands, failures, skips, and coverage
  limits explicitly; do not describe a build or helper test as runtime validation.
