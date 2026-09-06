# <img src="./docs/Waypoint-logo-notext.png" height="30" /> Waypoint

A lightweight checkpoint/restore tool that captures both filesystem and memory state with minimal overhead. 
Built on top of CRIU and OverlayFS for fast, isolated process state management.

> **Naming note:** Waypoint was previously called **Checkpoint-lite**. Some older blog posts, videos, and public announcements may still use the Checkpoint-lite name; they refer to the same project lineage.

## Overview 🌟

`waypoint` provides a simple interface to checkpoint running processes and restore them into concurrent live forks,
while capturing their memory state, live terminal sessions, and filesystem changes. Unlike heavyweight container
solutions, this tool focuses on minimal overhead by directly orchestrating existing kernel features and redesigning
terminal session management.

### Key Features

- **Hybrid State Capture**: Combines filesystem (OverlayFS) and memory (CRIU) checkpointing
- **Parallel Forking**: Non-destructively materialize many live, independently mutable forks from one immutable checkpoint
- **Terminal Session Support**: Preserves live terminal sessions and their state across checkpoints
- **Multi-Session Support**: Concurrent usage by multiple applications with isolated sessions
- **Minimal Overhead**: Direct system calls without unnecessary container abstractions
- **Minimal File IO**: Uses multiple lower-layer designs to achieve true inter-checkpoint deduplication
- **Simple CLI**: Straightforward command-line interface for checkpoint operations
- **Bash Completion**: Completes commands, flags, host paths, sessions, checkpoints, and live fork IDs
- **Session Management**: Automatic cleanup and resource management

## Architecture 🧱

### Design Philosophy

After analysis of existing checkpoint/restore solutions using our analysis tool [StateFork](https://github.com/Alex-XJK/StateFork)
and [StraceTools](https://github.com/Alex-XJK/stracetools), we identified that many traditional solutions often bundle 
unnecessary features like network isolation, security policies, and registry operations. 
`waypoint` takes a minimalist approach:

1. **Filesystem State**: Uses OverlayFS to capture directory changes without copying entire filesystems
2. **Memory State**: Leverages CRIU for process memory and execution state
3. **Terminal Sessions**: Implements a custom RPC-style PTY session management to preserve live terminal sessions across checkpoints
4. **Isolation**: Session-based isolation instead of full containerization
5. **Performance**: Direct tool orchestration minimizes call overhead

### Core Components

```
┌─────────────────┐    ┌─────────────────┐           ┌─────────────────┐
│   Filesystem    │    │     Memory      │ ───────── │   PTY Session   │
│   (OverlayFS)   │    │     (CRIU)      │           │   Management    │
└─────────────────┘    └─────────────────┘           └─────────────────┘
         │                       │
         └───────────┬───────────┘
                     │
            ┌─────────────────┐
            │   waypoint      │
            │   Session Mgr   │
            └─────────────────┘
```

- **OverlayFS Integration**: Creates layered filesystem views with minimal storage overhead
- **CRIU Orchestration**: Manages process memory dumping and restoration
- **PTY Session Management**: Uses an RPC-style approach to capture and communicate with terminal sessions
- **Session Manager**: Manages each session's checkpoint history and concurrent live forks

A **session** holds a checkpoint history and a set of live forks. A **checkpoint**
is an immutable snapshot of filesystem and process state; a **fork** is a running,
writable instance of that state. Each fork has a private filesystem layer and process
tree, so changes in one fork do not overwrite another fork or its source checkpoint.

### Go Language Technology Decision
The tool is implemented in Go for its simplicity, performance, and strong concurrency support.
See [our architecture decision record](./docs/tech_selection_note.md) for more details on why Go was chosen.

## Installation 🔧

Waypoint can be installed either through the project setup targets or manually.
For full details, see [Installing Waypoint](./docs/INSTALL.md).

### Scripted Setup (Recommended)

This path uses the repository `Makefile` to install system packages, build the
binaries, install the CLI/helper pair and Bash completion, and run a root-level
host check.

```bash
git clone https://github.com/Alex-XJK/waypoint.git
cd waypoint

# Ubuntu/Debian helper for host packages, CRIU, and Go. This mutates system state.
sudo make deps-ubuntu

make build
make test
sudo make install
sudo make check

sudo waypoint version
```

If you do not want to use `make`, the `./setup` script provides equivalent
commands, such as `./setup build`, `sudo ./setup install`, and
`sudo ./setup check`.

### Manual Installation

#### Prerequisites

- Linux system with root privileges
- CRIU installed and configured, including the `criu` and `crit` commands; CRIU 4.0 or newer is required on aarch64 by the default host check
- OverlayFS support (most modern Linux distributions)
- Go 1.25 or the version listed in `go.mod` (for building from source)
- Host utilities used by Waypoint: `cp` and `uname` (coreutils); teardown and mount handling are pure syscalls
- Optional: `buildah` for the build from Dockerfile approach (since v0.5.0)

#### Install Go (just for reference)

```bash
# Install Go (version 1.25.0)
wget https://go.dev/dl/go1.25.0.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.25.0.linux-amd64.tar.gz

# Add to ~/.bashrc or ~/.profile
export PATH=$PATH:/usr/local/go/bin
export GOPATH=$HOME/go
export GOBIN=$GOPATH/bin

# Reload shell
source ~/.bashrc

# Verify installation
go version
```

#### Install CRIU

```bash
# Ubuntu/Debian
sudo apt-get install criu
# or go to https://launchpad.net/~criu/+archive/ubuntu/ppa

# Verify installation
sudo criu check
```

#### Build from Source

```bash
git clone https://github.com/Alex-XJK/waypoint.git
cd waypoint
go build -o waypoint ./cmd/waypoint
# bash_init re-execs inside arbitrary session rootfses, so it must be built
# statically; waypoint refuses a dynamically linked bash_init at session start.
CGO_ENABLED=0 go build -o bash_init ./cmd/bash-init

# Install this pair, Bash completion, and the helper-path configuration
sudo env BIN_DIR="$PWD" ./setup install
```

#### Check Waypoint Version

```bash
sudo waypoint version
# Output: waypoint version v0.7.0
```

You can also run the root-level host check from the setup script after manual
installation:

```bash
sudo ./setup check
```

### Bash Completion (since v0.6.3)

The scripted installation includes Bash completion. In v0.7.0, it also completes
live fork IDs and the `fork-id:/` prefixes used by `cp`, alongside commands, flags,
session/checkpoint IDs, and local paths. Guest file paths and commands remain
user-supplied. See [Installing Waypoint](./docs/INSTALL.md#bash-completion) for
loading the helper in your shell.

## Usage 🗂

The examples below use the installed `waypoint` command with root privileges.
Since v0.7.0, `exec` selects a live fork explicitly, and returning to a checkpoint
creates a new fork with `fork` instead of overwriting the session with `restore`.

### 0. [Optional] Configure Global Settings

You can create a configuration file to set global options. Example content:
```json
{
  "sessions_dir": "/custom/path/waypoint-sessions",
  "session_info_dir": "/custom/path/waypoint-sessions-info",
  "bash_init_src": "/custom/compiled/bash_init",
  "preserve_session_on_cleanup": false,
  "tmpfs_images": false,
  "phase_stats": false
}
```

Configuration takes effect in the following order of precedence:

1. The direct environment variable `WAYPOINT_SESSIONS_DIR`, `WAYPOINT_SESSION_INFO_DIR`, `WAYPOINT_BASH_INIT_SRC`, `WAYPOINT_PRESERVE_SESSION_ON_CLEANUP`, etc. (if set)
2. Load from configuration file (if exists):
   - Explicit `WAYPOINT_CONFIG` environment variable
   - Binary-side config: `./config.json` (same dir as executable)
   - User config: `$XDG_CONFIG_HOME/waypoint/config.json` or `~/.waypoint/config.json`
   - System config: `/etc/waypoint/config.json`
3. Default settings.

Note: the default `sessions_dir` and `session_info_dir` live under `/tmp`, which is a tmpfs on many distros. Sessions that should survive a reboot (see `suspend`) need both configured onto durable storage.

The optional `tmpfs_images` setting (`WAYPOINT_TMPFS_IMAGES`) writes CRIU images to
tmpfs and flushes them to disk asynchronously; images are not durable until that
flush completes. `phase_stats` (`WAYPOINT_PHASE_STATS`) adds checkpoint/fork timing
breakdowns. Both options default to `false`.

### 1. Initialize Environment

#### 1.1. Initialize with Workspace

Create a managed environment from a root filesystem containing your application.
The example below starts the `main` shell fork; omit `--shell` to prepare only the
filesystem:

```bash
sudo waypoint init /path/to/your/workspace --shell
```

Output:
```
Environment initialized!
Session ID: a1b2c3d4e5f6g7h8
Work in this directory: /tmp/waypoint-sessions/a1b2c3d4e5f6g7h8/work
Shell PID: 1234 [socket: /proc/1234/root/tmp/waypoint-sessions/a1b2c3d4e5f6g7h8/temp/shell_a1b2c3d4e5f6g7h8.sock]

Save the session ID for future operations!
```

**Important**: Save the session ID and use `exec` and `cp` to work with a live fork.
Waypoint stages a copy of the supplied rootfs into the session; later edits to the
source directory are not edits to the running environment. The host-side work
directory is not a live view of every fork.

Special options:

- `--quiet` to output only the session ID and work directory, separated by a comma. (Since v0.2.1)
- `--shell` to start a shell in the managed environment immediately after initialization. (Since v0.5.0)
  - The provided rootfs must contain `/bin/bash`, its ELF loader and shared libraries, and any commands your application uses.

#### 1.2. Build Environment with Dockerfile (since v0.5.0)

You can alternatively build a sandbox environment directly with the `build` command, just like a Docker build.
This will set up a sandboxed environment with the provided Dockerfile and start
the `main` Bash fork in it.

```bash
sudo waypoint build /path/to/your/Dockerfile-directory
```

Output:
```
(Some build output from buildah...)
Sandbox environment built successfully!
Session ID: a1b2c3d4e5f6g7h8
Work in this directory: /tmp/waypoint-sessions/a1b2c3d4e5f6g7h8/work
Sandbox bash PID: 1234

Save the session ID for future operations!
```

Special options:

- `--quiet` to output only the session ID, work directory, and bash PID, separated by commas.

Since v0.6.3, sessions created with `build` apply the built image's `ENV` and
`WORKDIR` settings instead of silently replacing them with the host defaults.
The invoking host's environment is not passed through wholesale. Managed-shell
unattended settings take precedence over conflicting image values, and
`WAYPOINT_*` variables are reserved for internal use.

> Credit: This `buildah`-based workflow was originally designed by [Tianle Zhou](https://www.linkedin.com/in/tian-le-zhou-99a145221/)
in his TBench integration for v0.2.0.

### 2. Run Your Application

#### 2.1. Execute Shell Commands in a Fork

Initializing with `--shell` (or using `build`) gives you a live, isolated shell
called the `main` fork. You run commands in a fork with `exec`, and the fork
keeps its shell state (cwd, environment variables, background jobs) across calls:

```bash
sudo waypoint exec a1b2c3d4e5f6g7h8 main -- 'cat hello_world.txt'
sudo waypoint exec a1b2c3d4e5f6g7h8 main -- 'cd /app; export ENV_VAR=start'
```

Everything after `--` is one argument: the bash command line, parsed by the
fork's shell as `bash -c` would. Quote the whole command; several arguments are
refused rather than joined. Input bash cannot parse (an unterminated quote or
here-document, an open `if`) is refused with exit status 2 before it reaches the
fork; see [docs/exec-protocol.md](docs/exec-protocol.md#syntax-precheck).
`exec` exits with the command's own exit code. Commands on the same fork
serialize; commands on different forks run concurrently.

Since v0.6.3, managed shell sessions also use unattended defaults for common
pagers, editors, Git authentication, package-management tools, and Python
tooling so automation does not stall on an interactive prompt.

#### 2.2. Copy Files Between the Host and a Fork (since v0.7.0)

Use `cp` to bring preparation files into a running fork or retrieve its results:

```bash
sudo waypoint cp a1b2c3d4e5f6g7h8 ./preparation main:/app/preparation
sudo waypoint cp a1b2c3d4e5f6g7h8 main:/app/result.txt ./result.txt
```

Exactly one endpoint must use `fork-id:/absolute/path`; the other is a host path.
Destinations are exact paths: directory contents merge into the destination
directory, and regular files replace the destination file. Regular files and
directories are supported; source symlinks and special files are not. Copies
preserve file permission bits, but not ownership or timestamps.

### 3. Create Checkpoints

A checkpoint saves a fork's filesystem and memory state as an immutable node in the
session's history. `checkpoint` saves the `main` fork; use `snapshot` to select a
different fork (see section 4).

```bash
sudo waypoint checkpoint a1b2c3d4e5f6g7h8 checkpoint-name
```

Checkpoint IDs must be new names within the session. The legacy `create` command
remains an alias for `checkpoint`; it also always selects `main`.

### 4. Fork and Snapshot

`fork` materializes a live, writable instance from any checkpoint. Fork the same
checkpoint several times to explore divergent branches in parallel — each fork
gets its own private copy-on-write filesystem and its own process tree:

```bash
sudo waypoint fork a1b2c3d4e5f6g7h8 checkpoint-name --id f1
sudo waypoint fork a1b2c3d4e5f6g7h8 checkpoint-name --id f2
```

For a batch of concurrent forks with generated IDs, use `--n`:

```bash
sudo waypoint fork a1b2c3d4e5f6g7h8 checkpoint-name --n 4
```

The command prints each new fork ID. `--id` names a single fork and cannot be
combined with a count greater than one.

`snapshot` seals a live fork's current state into a new checkpoint, then resumes
that fork on fresh writable layers. The checkpoint can itself be forked again,
so the same operations build successive branches of the checkpoint history.
`destroy` tears down a selected fork and discards its unsaved changes:

```bash
sudo waypoint snapshot a1b2c3d4e5f6g7h8 f2 checkpoint-name-2
sudo waypoint destroy a1b2c3d4e5f6g7h8 f1
```

To save a fork without resuming it, add `--park`. The checkpoint remains available
for a later `fork`; `main` cannot be parked:

```bash
sudo waypoint snapshot a1b2c3d4e5f6g7h8 f2 checkpoint-name-3 --park
```

To return to an earlier checkpoint, create another fork from it and direct later
`exec` and `cp` calls to that fork. Existing forks are left intact:

```bash
sudo waypoint fork a1b2c3d4e5f6g7h8 checkpoint-name --id retry
sudo waypoint exec a1b2c3d4e5f6g7h8 retry -- 'echo "Back at the saved state"'
```

### 5. List Available Sessions, Checkpoints, and Forks

```bash
sudo waypoint list
sudo waypoint list a1b2c3d4e5f6g7h8
sudo waypoint list a1b2c3d4e5f6g7h8 --json
```

Without a session ID, `list` shows all recorded session IDs. With a session ID,
it shows that session's checkpoints and its live forks; add `--json` for the
stable machine-readable shape.

### 6. Inspect System, Session, and Checkpoint Info

```bash
sudo waypoint info
sudo waypoint info a1b2c3d4e5f6g7h8
sudo waypoint info a1b2c3d4e5f6g7h8 checkpoint-name
```

The `info` command prints JSON for system/configuration details, a specific session, or a specific checkpoint.

### 7. Suspend Session

```bash
sudo waypoint suspend a1b2c3d4e5f6g7h8
```

Ends the session's live compute while retaining its checkpoint history. Running
forks are destroyed and their unsaved changes are discarded — `snapshot` a fork
first if you want to keep its latest state. The session stays registered, and
`sudo waypoint fork <session> <checkpoint>` creates a live fork from a saved
checkpoint later.

For use after a reboot, both session directories must be on durable storage
(see section 0), and any tmpfs images must have finished flushing to disk. Check
for teardown or flush warnings before relying on suspension or disk durability.

### 8. Clean Up Session

```bash
sudo waypoint cleanup a1b2c3d4e5f6g7h8
```
If this basic version of the cleanup command fails, **waypoint** will automatically suggest further actions. You can use:

- `--force` to forcefully remove and unmount all related resources.

For debugging, set `preserve_session_on_cleanup` to `true` in the config file, or set `WAYPOINT_PRESERVE_SESSION_ON_CLEANUP=true`. Cleanup will still stop processes and unmount resources, but it will keep the session directory and session registry entry for inspection.

## Demo 🎥

- **Waypoint Videos** – Subscribe to the [Waypoint YouTube playlist](https://www.youtube.com/playlist?list=PLQLBrJURYsZQ) for demos and updates.

For executable examples with assertions, run:

```bash
sudo ./scripts/demo.sh
sudo ./scripts/test-search-workflow.sh
```

The demo covers the CLI lifecycle. The search workflow uses an offline Dockerfile
and exercises preparation copies, concurrent candidates, recursive snapshots,
backtracking, result export, and cleanup.

## Example Workflow 🧩

### Parallel Forking with a Shell Session

This example assumes the Dockerfile supplies `/app/run-app.sh` and that the host
has a `./preparation` directory to copy into the environment. Replace the example
session ID with the one returned by `build`.

```bash
# Initialize environment using a Dockerfile
sudo waypoint build /home/docker-tasks/context
## (Some build output from buildah...)
## Sandbox environment built successfully!
## Session ID: abc123def456
## Work in this directory: /tmp/waypoint-sessions/abc123def456/work
## Sandbox bash PID: 123456
##
## Save the session ID for future operations!

# Copy preparation files into main before checkpointing
sudo waypoint cp abc123def456 ./preparation main:/app/preparation

# Build up some hidden shell state in the main fork
sudo waypoint exec abc123def456 main -- 'cd /app; export ENV_VAR=start'

# Checkpoint the main fork into an immutable checkpoint
sudo waypoint checkpoint abc123def456 before-run
## Checkpoint 'before-run' created successfully

# Create two live branches from the same saved state
sudo waypoint fork abc123def456 before-run --id runA
sudo waypoint fork abc123def456 before-run --id runB

# Each fork inherits the state, then runs independently and concurrently
sudo waypoint exec abc123def456 runA -- './run-app.sh --mode fast && export ENV_VAR=finished-A' &
run_a_pid=$!
sudo waypoint exec abc123def456 runB -- './run-app.sh --mode slow && export ENV_VAR=finished-B' &
run_b_pid=$!

# Wait for both CLI calls; each wait reports that command's exit status
wait "$run_a_pid"
wait "$run_b_pid"

sudo waypoint exec abc123def456 runA -- 'echo VALUE: $ENV_VAR PWD: $(pwd)'
## VALUE: finished-A PWD: /app
sudo waypoint exec abc123def456 runB -- 'echo VALUE: $ENV_VAR PWD: $(pwd)'
## VALUE: finished-B PWD: /app

# Snapshot runA's diverged state and continue in a new descendant
sudo waypoint snapshot abc123def456 runA after-run-A
## Fork 'runA' snapshotted as checkpoint 'after-run-A'
sudo waypoint fork abc123def456 after-run-A --id runA-next
sudo waypoint exec abc123def456 runA-next -- 'echo Inherited: $ENV_VAR'
## Inherited: finished-A

# Backtrack independently to the original state
sudo waypoint fork abc123def456 before-run --id retry
sudo waypoint exec abc123def456 retry -- 'echo Original: $ENV_VAR'
## Original: start

# Copy the selected branch's files to the host before cleanup
sudo waypoint cp abc123def456 runA:/app ./results/runA

# Inspect the checkpoint/fork DAG
sudo waypoint list abc123def456

# Clean up when done (kills forks, unmounts overlays, removes the session)
sudo waypoint cleanup abc123def456
```

## Directory Structure 🗃

```
/custom/path/waypoint-sessions/    # Configured sessions directory
    └── abc123def456/               # A session
        ├── original/               # Session-owned rootfs, shared read-only
        ├── checkpoints/            # Immutable checkpoint DAG nodes
        │   └── before-run/
        │       ├── upper/          # Sealed, immutable filesystem delta
        │       └── criu/           # CRIU memory image (+ dump.log)
        │           └── *.img
        ├── forks/                  # Live, mutable fork instances
        │   ├── main/               # The main fork (from init --shell or build)
        │   │   ├── fork.json       # Live fork record
        │   │   └── upper/  work/   # This fork's private CoW layers
        │   ├── runA/
        │   │   ├── fork.json
        │   │   ├── upper/  work/
        │   │   └── restore.log
        │   └── runB/
        ├── metadata/               # Checkpoint metadata
        │   └── before-run.json     # "Metadata" for the before-run checkpoint
        ├── locks/                  # Session/fork flocks
        ├── temp/                   # Main shell startup/restore log
        └── work/                   # Canonical merged mountpoint (per-fork)

/custom/path/waypoint-sessions-info/ # Configured global session registry
    └── abc123def456.json           # "SessionInfo" for the session above
```

The merged `work/` path is mounted separately in each fork's namespace. The shell
control socket uses a canonical path inside that view; host communication resolves
it through the running fork's `/proc/<pid>/root`. Use `cp` to transfer live files
instead of treating the host-side `work/` directory as the selected fork. With
`tmpfs_images` enabled, a checkpoint's `criu` entry is a symlink to its active image
location while images are flushed to disk.

## Technical Details ⌨️

The original v0.6.0 diagram is retained for reference; the sections below describe
the v0.7.0 checkpoint/fork design.

![Historical v0.6.0 Architecture Diagram](./docs/waypoint-v060.drawio.png)

### OverlayFS Initialization

- **Lower Layers**: The session-owned rootfs in `original/` plus the checkpoint chain's sealed upper layers (read-only), with newer layers taking priority
- **Upper Layer**: This fork's private changes (copy-on-write)
- **Work Layer** (`forks/<fork>/work/`): Temporary storage for OverlayFS internal operations
- **Merged View** (`work/`): Combines the fork's upper with the lower layers for the process to see

### Snapshot (fork → checkpoint)

- **CRIU Dump**: Dumps the fork's process memory, file descriptors, and execution state
- **Seal Upper**: Moves the fork's upper layer into the checkpoint as an immutable, shareable delta
- **Rebase**: Gives the fork fresh upper/work layers stacked on the new checkpoint and resumes it; `--park` keeps only the checkpoint without resuming the fork
- **Metadata Management**: Records the checkpoint's `ParentID` and resolved `LayerIDs` chain

### Fork (checkpoint → live fork)

- **Non-destructive**: The source checkpoint is immutable, so one checkpoint can be forked many times
- **Private Overlay**: Each fork mounts its own overlay (checkpoint layers + a private upper) in its own mount namespace
- **CRIU Restore**: Rebuilds the fork's process tree from the checkpoint's memory image; the command waits for readiness, and different forks can be restored concurrently

### Session Isolation
Each session gets:

- Unique randomly generated session ID
- Isolated directory structure
- Checkpoint IDs scoped to the session
- Shared immutable layers with a private writable overlay for each live fork
- A dedicated shell supervisor and process tree for each live fork

Forks use separate filesystem views and process namespaces. Networking is shared
with the host by design; forks do not receive private IP addresses or separate
port spaces.

### Terminal Session Management

- **RPC server**: A controlling process that manages a PTY session and listens for commands via Unix domain socket
- **Isolated bash core**: A long-running Bash session supervised by `bash_init`, which enters the fork's filesystem with `pivot_root` and runs as its namespace init
- **RPC-style communication**: The shell supervisor receives commands, forwards them to Bash, and returns output and exit status; a separate completion FIFO keeps control messages out of application output
- **RPC client**: The main waypoint process acts as a client to send commands to the bash server

> Credit: This is an iterated version of the command injection method implemented by 
[Georgios Liargkovas](https://liargkovas.com/) in the v0.4.0 series. It was first designed and trialed by 
[Alex Jiakai Xu](https://alex-xjk.github.io/) in his [pty-rpc-shell](https://github.com/Alex-XJK/pty-rpc-shell) side project.

## Limitations

- Requires root privileges (CRIU and OverlayFS requirement)
- Linux-specific (depends on CRIU and OverlayFS)
- Forks share host networking, so services using the same ports can conflict; network connections may not survive checkpoint/restore
- Exec uses one persistent PTY: stdout/stderr are combined, and output is returned when the command finishes rather than streamed
- A checkpoint captures supported process/filesystem state; it does not provide a full security boundary for untrusted workloads

## Citation

If you use waypoint in academic research, please cite:
```bibtex
@misc{xu2025systemsfoundationsagenticexploration,
      title={Toward Systems Foundations for Agentic Exploration}, 
      author={Jiakai Xu and Tianle Zhou and Eugene Wu and Kostis Kaffes},
      year={2025},
      eprint={2510.05556},
      archivePrefix={arXiv},
      primaryClass={cs.DC},
      url={https://arxiv.org/abs/2510.05556}
}
```
