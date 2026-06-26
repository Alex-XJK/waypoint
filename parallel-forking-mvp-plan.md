# Parallel Forking MVP Plan

This is the pickup plan for turning the validated parallel-forking prototype into a
clean MVP interface. The goal is correctness and a crisp model for recursive container
forking. Millisecond-level optimization comes later behind the same primitives.

## Objective

Make Waypoint's public model:

```text
Session = checkpoint store + live fork registry
Checkpoint = immutable DAG node
Fork = live mutable instance of one checkpoint
Snapshot = live fork -> new checkpoint
Fork = checkpoint -> live fork
```

Recursive forking should be ordinary:

```bash
waypoint fork <session> ckptA --id fork1
waypoint exec <session> fork1 -- 'echo branch > /tmp/state'
waypoint snapshot <session> fork1 ckptB
waypoint fork <session> ckptB --id fork2
waypoint fork <session> ckptB --id fork3
```

## Scope

Focus on these first:

1. Replace checkpoint metadata with clean DAG/layer-chain fields.
2. Treat the main shell as a fork, not as session-global mutable state.
3. Make materialization use checkpoint layer chains for OverlayFS lowerdirs.
4. Implement snapshot-from-fork, including rebase of the live fork.
5. Add file locks so commands on the same fork serialize and different forks can run
   concurrently.

Compatibility with the old `current`/`create`/`restore` flow is not required for this
implementation pass. Keep old commands only if they are cheap aliases over the new model.

## Target CLI

Preferred MVP interface:

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

Recommended conventions:

- `init --shell` creates a live fork named `main`.
- `checkpoint <session> <id>` snapshots `main`.
- `exec <session> <fork-id> -- <command>` is the canonical execution form.
- `restore` is legacy-only and should not be part of the parallel API.
- `create` can become an alias for `checkpoint` if useful.
- `fork-exec` can become an alias for `exec` if useful.

## Core Data Model

Use explicit checkpoint and fork records. Suggested structs:

```go
type CheckpointMetadata struct {
    ID                string   `json:"id"`
    ParentID          string   `json:"parent_id,omitempty"`
    LayerIDs          []string `json:"layer_ids"`
    PID               int      `json:"pid"`
    OriginalDir        string   `json:"original_dir"`
    SessionID          string   `json:"session_id"`
    CreatedFromForkID  string   `json:"created_from_fork_id,omitempty"`
    CreatedAt          int64    `json:"created_at"`
    Status            string   `json:"status"` // creating, ready, failed
}

type Fork struct {
    ID                string     `json:"id"`
    SessionID         string     `json:"session_id"`
    BaseCheckpointID  string     `json:"base_checkpoint_id"`
    LayerIDs          []string   `json:"layer_ids"`
    OriginalDir        string     `json:"original_dir"`
    RootDir           string     `json:"root_dir"`
    UpperDir          string     `json:"upper_dir"`
    WorkDir           string     `json:"work_dir"`
    MountPoint        string     `json:"mount_point"`
    PID               int        `json:"pid"`
    SocketPath        string     `json:"socket_path"`
    CanonicalSocket   string     `json:"canonical_socket"`
    Status            ForkStatus `json:"status"`
}
```

`ParentID` is the logical DAG edge. `LayerIDs` is the resolved filesystem chain from
oldest to newest and should include the checkpoint itself for checkpoints, or all base
checkpoint layers for forks. Store `LayerIDs` so materialization does not need to walk the
DAG on every fork.

Example:

```text
A: ParentID="",  LayerIDs=[A]
B: ParentID=A,   LayerIDs=[A,B]
C: ParentID=B,   LayerIDs=[A,B,C]
```

Forking checkpoint `C` mounts:

```text
upperdir=<session>/forks/f1/upper
workdir=<session>/forks/f1/work
lowerdir=<session>/C/upper:<session>/B/upper:<session>/A/upper:<original-rootfs>
```

Overlay lowerdir priority is leftmost-highest, so always reverse `LayerIDs` when building
lowerdirs.

## Filesystem Layout

Suggested session layout:

```text
<session>/
  session.json
  metadata/
    <checkpoint-id>.json
  checkpoints/
    <checkpoint-id>/
      upper/
      criu/
      checkpoint.json
  forks/
    <fork-id>/
      fork.json
      upper/
      work/
      temp/
      restore.log
      restore.pid
  locks/
    session.lock
```

Using `checkpoints/<id>` is cleaner than top-level `<id>` directories, but the exact move
can be staged if needed. Greenfield code should avoid adding new dependencies on
top-level checkpoint directories.

## Locking Model

Use file locks because each CLI command is a separate process. Prefer `flock(2)` via
`golang.org/x/sys/unix.Flock`.

Locks:

```text
<session>/locks/session.lock
<session>/forks/<fork-id>/lock
```

Rules:

- The session lock protects checkpoint ID reservation, checkpoint metadata commits, fork
  ID allocation, and session registry writes.
- A fork lock protects one live fork's shell/socket/PID/upper/work dirs.
- `exec` takes the fork lock for the full command.
- `snapshot` takes the fork lock for the full snapshot and rebase.
- `destroy` takes the fork lock for the full teardown.
- `fork <checkpoint>` takes the session lock briefly to allocate and persist the fork
  record, then uses only the new fork's lock while restore runs.
- Different forks should be able to execute concurrently.
- Never hold `session.lock` while waiting for an existing fork's lock.

Deadlock discipline:

1. If an operation needs both, take the fork lock first only when it already owns the fork
   action; take the session lock only for short metadata commits.
2. Do not call into long-running CRIU, shell exec, mount, or process-kill work while
   holding the session lock.
3. Keep metadata writes atomic: write `*.tmp`, `fsync` if practical, then rename.

## Snapshot Flow

Snapshotting a fork creates a new immutable checkpoint and rebases the live fork onto it.

High-level flow:

```text
lock fork
  lock session
    reserve checkpoint ID with status=creating
  unlock session

  dump fork PID with CRIU into a staging checkpoint dir
  seal fork upper as checkpoint upper
  restore/rebase fork onto the new checkpoint with fresh fork upper/work dirs

  lock session
    commit checkpoint metadata status=ready
    update fork record
  unlock session
unlock fork
```

After snapshotting `fork1` based on checkpoint `B` into `C`:

```text
C.ParentID = B
C.LayerIDs = fork1.LayerIDs + [C]
fork1.BaseCheckpointID = C
fork1.LayerIDs = C.LayerIDs
fork1.UpperDir = fresh empty upper
fork1.WorkDir = fresh empty work
```

The fork remains live after snapshot. This mirrors the old `create` behavior but is scoped
to one fork and never mutates unrelated forks.

Implementation detail options for sealing fork upper:

1. Preferred MVP: stop/dump the fork, unmount its overlay, move `fork.upper` into the
   checkpoint dir, create fresh fork upper/work, remount/restore fork from the checkpoint.
2. Later optimization: copy/reflink the upper while preserving the live fork upper, then
   avoid a full rebase when possible.

## Fork Materialization Flow

Materializing a checkpoint:

```text
lock session
  reserve fork ID and write fork.json status=starting
unlock session

lock new fork
  create fork upper/work/temp dirs
  mount overlay at canonical mountpoint in fresh mount namespace using LayerIDs
  restore CRIU image with --restore-detached and --pidfile
  wait for /proc/<pid>/root/<canonical-socket> to become dialable
  write fork.json status=running
unlock new fork
```

The CRIU materializer remains the correctness baseline. Faster strategies must implement
the same `Materializer` shape later.

## Execution Flow

```text
load fork
lock fork
  verify status=running
  dial fork.SocketPath
  send command
  return output
unlock fork
```

This intentionally serializes commands on the same fork and permits commands on different
forks to run concurrently.

## Destroy Flow

```text
load fork
lock fork
  mark status=destroying
  kill namespace init host PID
  remove socket
  unmount fork mounts
  mark status=destroyed
  remove fork dir, or keep failed logs if teardown fails
unlock fork
```

Cleanup should iterate live forks and destroy them before removing session-level state.

## List/Inspect

`waypoint list <session> --json` should be stable enough for StateFork and other callers:

```json
{
  "session_id": "...",
  "checkpoints": [
    {"id": "A", "parent_id": "", "layer_ids": ["A"], "status": "ready"}
  ],
  "forks": [
    {"id": "main", "base_checkpoint_id": "A", "status": "running", "pid": 1234}
  ]
}
```

The human text format can be lightweight. The JSON shape is the contract.

## Implementation Order

1. Add lock helpers:
   - `withSessionLock(fn func() error) error`
   - `withForkLock(forkID string, fn func() error) error`
   - atomic JSON write helper.
2. Replace metadata and fork records with `ParentID`, `LayerIDs`, and status fields.
3. Convert `init --shell` to create `main` fork state.
4. Update overlay construction to use `LayerIDs`.
5. Update `fork` to materialize from checkpoint metadata and persist status transitions.
6. Update `exec` and `destroy` to use fork locks.
7. Implement `snapshot <session> <fork-id> <checkpoint-id>`.
8. Add `list --json`.
9. Runtime validate:
   - init/main exec
   - snapshot main -> A
   - fork A -> f1/f2
   - divergent writes stay isolated
   - snapshot f1 -> B
   - fork B -> f3
   - f3 sees f1's snapshot state, f2 and main do not
   - destroy each fork
   - cleanup leaves no mounts

## Validation Commands

Use a writable Go cache in this environment:

```bash
env GOCACHE=/tmp/waypoint-go-cache go test ./...
env CGO_ENABLED=0 GOCACHE=/tmp/waypoint-go-cache go build -o bash_init ./cmd/bash-init
env GOCACHE=/tmp/waypoint-go-cache go build -o waypoint ./cmd/waypoint
sudo criu check
```

Runtime validation requires root on Linux. If root-owned sessions are created, always clean
them:

```bash
sudo ./waypoint cleanup <session> --force
sudo findmnt -R <session-dir>
```

## Deferred Until After MVP

- Reflink copy-up for faster snapshot/fork.
- Lazy-pages page server as a default path.
- tmpfs CRIU image cache and donor pool.
- KSM/background dedup.
- `fork()` template materializer.
- outbound networking/proxy policy.
- old API compatibility polish.
