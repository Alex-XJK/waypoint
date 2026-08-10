# Waypoint Architecture

Waypoint materializes many live, isolated, observation-equivalent copies of a
terminal execution session from a single checkpoint — "fork a running shell,
with its cwd, variables, background jobs, and local servers intact." It composes
three Linux facilities:

- **OverlayFS** for copy-on-write filesystem layering (shared read-only lower
  layers + a private writable upper per fork).
- **CRIU** for checkpoint/restore of a process tree's memory and kernel state.
- **A persistent PTY-backed shell** (`bash_init`) that is the thing being
  checkpointed, so hidden process state survives.

## The mental model

```
Session  = a checkpoint store + a live fork registry   (one workspace)
Checkpoint = an immutable node in a DAG                 (frozen state)
Fork     = a live, mutable instance of one checkpoint   (a running shell)

  checkpoint --fork--> live fork --snapshot--> new checkpoint --fork--> ...
```

- **Checkpoint**: sealed OverlayFS upper layer + CRIU memory image. Immutable.
  Nodes form a DAG via `ParentID`; `LayerIDs` is the resolved layer chain.
- **Fork**: its own upper/work dirs, its own PID+mount+net+IPC namespaces, its
  own restored shell + control socket. Many can run at once from one checkpoint.
- **`main`** is just the first fork, created by `init --shell`.
- **snapshot** seals a live fork into a new checkpoint and rebases the fork
  onto it — this is what makes recursive forking ordinary. The CLI's
  `checkpoint <session> <id>` is shorthand for snapshotting `main`;
  `fork --n K` materializes K forks off one checkpoint in one command.

## Component map

```
                          waypoint CLI  (cmd/waypoint/main.go)
                          parses argv, one Manager per command
                                     |
                                     v
        +----------------------------------------------------------+
        |                    Manager  (pkg/waypoint)               |
        |   session paths, checkpoint store, fork registry, locks  |
        +----------------------------------------------------------+
          |            |              |            |            |
          v            v              v            v            v
     Materializer  Filesystem     Memory       Fork/       Locks +
     (CRIU C/R)    (OverlayFS)    (CRIU dump)  Metadata     Session
          |            |              |         records      registry
          |            |              |
          |   builds the overlay a fork's shell sees
          |
          | restore helper re-enters as a namespaced child
          v
   __waypoint_restore_fork_child  ---->  criu restore
                                              |
                                              v
                              bash_init  (cmd/bash-init/main.go)
                              PID 1 of the fork's namespaces
                                  |
                                  +-- /bin/bash on a PTY  (the shell)
                                  +-- Unix socket  (command RPC, protocol v2)
                                  +-- /.waypoint/exec.done FIFO (completion)
```

Two binaries, one library:

| Binary | Role |
|---|---|
| `cmd/waypoint` | The CLI. Thin argv dispatch over the `waypoint` package. Also hosts the hidden re-exec entry points: `__waypoint_restore_fork_child` (namespaced restore helper) and `__waypoint_flush_images` (background tmpfs-image flusher). |
| `cmd/bash-init` | The in-container shell supervisor. Staged inside each session's overlay, checkpointed and restored as part of every fork. See `docs/exec-protocol.md`. |
| `pkg/waypoint` | Everything else: session/checkpoint/fork lifecycle, CRIU and OverlayFS plumbing, locking. |

## File-by-file (`pkg/waypoint`)

- `manager.go` — the `Manager` handle and everything session-level:
  `NewManagerWithSession` (mint a session), `LoadManager` (rehydrate one by ID),
  the global session registry under `/tmp/waypoint-sessions-info/`
  (`LoadSessionInfo`, `ListSessions`), the on-disk layout (path helpers),
  `flock`-based `withSessionLock` / `withForkLock` (cross-process, because each
  CLI invocation is a separate process), `atomicWriteFile`, and
  `ListSession` / `SessionListing` (the stable `list --json` shape).
- `config.go` — configuration loading (`loadConfig`: env/file overrides, each
  tracked back to its source) and `LoadConfigInfo` / `ConfigInfo` for the
  `info` command. Knobs: sessions dir, bash_init path,
  `preserve_session_on_cleanup`, `tmpfs_images` + `tmpfs_images_dir` (see
  `imagestore.go`), and `phase_stats` (see `criustats.go`).
- `checkpoint.go` — immutable checkpoints: `Metadata` (checkpoint record:
  `ParentID`, `LayerIDs`, `PID`, `Status`) and its JSON persistence, plus the
  `Materializer` interface + `CRIUMaterializer`:
  - `Materialize` — checkpoint -> new live fork (allocate record under the
    session lock, then restore under the fork lock).
  - `snapshotFork` — dump the fork, seal its upper into a checkpoint, rebase the
    fork onto the new checkpoint. With park (`snapshot --park` / `ParkFork`),
    skip the re-restore and release the fork instead: the node survives only as
    the checkpoint (cheapest persist; revive later with `fork`). `main` cannot
    be parked.
- `fork.go` — live forks: the `Fork` record (paths, socket, PID, status) and
  its persistence; `newForkRecord` (a fresh fork off a checkpoint),
  `saveMainFork` (record `main` after `init --shell`), `DestroyFork`; and the
  client half of the exec protocol (`ExecuteForkCommand` -> `execCommand`: dial
  the fork's socket, send a length-prefixed command, parse the
  `WP2 <status> <code>` response with v1 fallback). Server half lives in
  `cmd/bash-init`.
- `criu.go` — every criu(8) interaction: `createMemoryCheckpoint` (the
  `criu dump` command, including the `--external mnt[...]:waypoint-work` mapping
  so CRIU treats the overlay as externally managed); and the restore side —
  `runRestoreHelper` / `RunForkRestoreChildFromArgs` / `restoreForkChild`
  re-exec into fresh mount/net/IPC namespaces, mount this fork's overlay at
  the canonical path, and `criu restore` the image. Host compatibility (criu
  present, recent enough, kernel features, the ARM64 PAC / CRIU-4.0 rule) is
  validated out of band by `./setup check`, not at runtime.
- `overlay.go` — the CoW layer: `InitEnvironment` (first overlay mount for
  `main`), the single `mountOverlay` used both on the host and inside a
  restore child's namespace, runtime pseudo-fs mounts (`/proc`, `/sys`), and
  `overlayLowerDirs` (turn a `LayerIDs` chain into ordered overlay lowerdirs:
  newest checkpoint first, original rootfs last).
- `imagestore.go` — the `tmpfs_images` fast path: `prepareCheckpointImagesDir`
  makes `checkpoints/<ckpt>/criu` a symlink into a tmpfs dir the dump writes
  to; `spawnImageFlusher` / `RunImageFlushFromArgs` / `FlushCheckpointImages`
  copy the images to `checkpoints/<ckpt>/criu.disk` in the background and
  atomically repoint the symlink; an images `flock` keeps restores and the
  flusher from racing. With the flag off, `criu` is a plain directory.
- `criustats.go` — the `phase_stats` instrumentation: `RestoreBreakdown` /
  `SnapshotBreakdown` (persisted in fork.json / checkpoint metadata, printed
  as flat `key_ms=` tokens) and a minimal protobuf-varint parser for CRIU's
  `stats-dump` / `stats-restore` images.
- `cleanup.go` — the three `Cleanup*` variants (graceful, forced, interactive),
  process existence/kill helpers, and the force-cleanup crew
  (`lsof`/`fuser`/`findmnt`-based).
- `build.go` — `StartShell` (stage `bash_init` into the overlay and launch it
  namespaced), `BuildEnvironment` / `BuildFromDockerfile` (buildah-based rootfs
  build), `prepareDevNodes` (seed the rootfs's own `/dev`: basic char devices
  plus a sticky `shm/` dir; `bash_init` completes it at session start — see
  the `init --shell` walkthrough), and dependency-staging helpers (`ldd` ->
  copy libs into rootfs).

## On-disk layout

```
/tmp/waypoint-sessions/<session>/
  work/                     canonical merged mountpoint (mounted per-fork,
                              differently, in each fork's mount namespace)
  temp/
    shell_<session>.sock    canonical control-socket path (main's; each fork
    shell_<session>.log       has its own socket at this path inside its ns)
  metadata/<ckpt>.json      checkpoint DAG nodes
  checkpoints/<ckpt>/
    upper/                  sealed, immutable filesystem delta
    criu/                   CRIU memory image (+ dump.log); with tmpfs_images
                              a symlink (tmpfs first, criu.disk/ once flushed)
  forks/<fork>/
    fork.json               live fork record
    upper/  work/  temp/    this fork's private CoW layers + socket dir
    lock                    per-fork flock
    restore.log restore.pid
  locks/session.lock        session-wide flock

/tmp/waypoint-sessions-info/<session>.json   global registry (find a session)
/dev/shm/waypoint/<session>/<ckpt>/          tmpfs images (tmpfs_images only)
```

Overlay lowerdir order for a fork on checkpoint chain `[A,B,C]`:

```
lowerdir = C/upper : B/upper : A/upper : <original rootfs>   (highest prio left)
upperdir = forks/<fork>/upper      (private, writable)
```

## How the tree evolves

The layout above is a steady-state snapshot. What makes the design legible is
how few filesystem operations ever mutate it: every command is some
combination of *make empty dirs*, *mount an overlay*, *criu dump/restore*, and
exactly one `rename(2)`. Below, one session (`S`) walks through the whole
lifecycle. `(empty)` marks a directory that exists but has no entries; `◄` marks
what changed in that step.

### Step 1 — `init <rootfs> --shell`: one fork, zero checkpoints

```
/tmp/waypoint-sessions/S/
├── work/                        ◄ overlay MOUNTED here (host mount, main only)
│                                    lowerdir = <rootfs>
│                                    upperdir = forks/main/upper
│                                    workdir  = forks/main/work
├── temp/
│   ├── shell_S.sock             ◄ main's control socket (canonical path)
│   └── shell_S.log
├── metadata/                    (empty — no checkpoints yet)
├── checkpoints/                 (empty)
├── forks/
│   └── main/
│       ├── fork.json            ◄ PID + socket of the running shell
│       ├── upper/               ◄ every write the shell makes lands here
│       ├── work/                  (overlayfs scratch space)
│       └── temp/
└── locks/session.lock

/tmp/waypoint-sessions-info/S.json   ◄ global registry entry
```

`main` is an ordinary fork that happens to have no base checkpoint: its
`LayerIDs` is empty, so its overlay is just `rootfs + main/upper`. The shell
runs *inside* the mounted `work/` tree (pivot_root), so `work/` is both the
mountpoint and the root the checkpointed process believes is `/`.

### Step 2 — `snapshot S main A`: seal main's delta, rebase main onto it

The core move is a single atomic rename — the fork's upper dir *becomes* the
checkpoint's layer. Nothing is copied.

```
1. criu dump main's tree     ─────►  checkpoints/A/criu/   (memory image)
2. unmount work/                     (main's host mount only)
3. rename forks/main/upper   ─────►  checkpoints/A/upper   ◄ THE seal (rename(2))
4. recreate forks/main/{upper,work,temp} empty
5. criu restore main from A          (in fresh namespaces; work/ now mounts
                                      lowerdir = A/upper : <rootfs>)
```

```
/tmp/waypoint-sessions/S/
├── work/                        (mounted per-fork in namespaces from here on)
├── metadata/
│   └── A.json                   ◄ {ParentID:"", LayerIDs:["A"], PID, Status:ready}
├── checkpoints/
│   └── A/
│       ├── upper/               ◄ was forks/main/upper — now sealed, immutable
│       └── criu/                ◄ CRIU image + dump.log
└── forks/
    └── main/
        ├── fork.json            ◄ rebased: BaseCheckpointID=A, LayerIDs=[A]
        ├── upper/               ◄ fresh + empty — main's writes start over
        ├── work/                  (recreated)
        └── temp/
```

### Step 3 — `fork S A --id f1`: a second live instance of A

Forking touches only `forks/` — checkpoints are never written after sealing.

```
/tmp/waypoint-sessions/S/
├── checkpoints/
│   └── A/  upper/ criu/         (untouched, shared read-only by main and f1)
└── forks/
    ├── main/  ...               (still running, unaffected)
    └── f1/                      ◄ all new
        ├── fork.json
        ├── upper/               ◄ f1's private writes (empty at birth)
        ├── work/
        ├── temp/
        │   └── shell_S.sock     ◄ f1's own socket, reached via /proc/<pid>/root/...
        ├── restore.log
        └── restore.pid
```

Both forks mount an overlay at the *same canonical path* `<session>/work` — but
each in its own mount namespace, so the mounts don't collide:

```
main's namespace:  work = main/upper  over  A/upper  over  rootfs
f1's   namespace:  work = f1/upper    over  A/upper  over  rootfs
                                            ~~~~~~~~~~~~~~~~~~~~~ shared, read-only
```

### Step 4 — `snapshot S f1 B`: recursive forking is just step 2 again

```
f1/upper ──rename──► checkpoints/B/upper      metadata/B.json:
f1 rebased: LayerIDs=[A,B], fresh empty upper   ParentID: A
                                                LayerIDs: [A, B]
```

Now the checkpoint DAG and the layer chains diverge per branch:

```
        (rootfs)
           │
           A ──── main still runs on [A]        work = main/upper : A/upper : rootfs
           │
           B ──── f1 now runs on [A,B]          work = f1/upper : B/upper : A/upper : rootfs
```

### Step 5 — `snapshot S f1 C --park`: persist without resuming

Park is steps 1–3 of a snapshot, then deletion instead of restore:

```
1. criu dump f1            ─────►  checkpoints/C/criu/
2. rename f1/upper         ─────►  checkpoints/C/upper
3. rm -rf forks/f1/                ◄ fork record, socket, logs — all gone
```

```
├── metadata/    A.json  B.json  C.json      (C: ParentID=B, LayerIDs=[A,B,C])
├── checkpoints/ A/      B/      C/          ◄ C is now the ONLY trace of f1
└── forks/       main/                       ◄ f1 has left the registry
```

The node survives purely as data. `fork S C --id f2` later revives it —
which is just step 3, so the revived fork gets a new empty upper on chain
`[A,B,C]`. (`main` can't be parked; the session needs one live fork.)

### Invariants worth internalizing

- **`checkpoints/<id>/upper` is written exactly once** — by the rename that
  sealed it. After that it only ever appears on the read-only side of
  overlay mounts. Deleting a checkpoint that others chain on would corrupt
  every descendant, which is why nodes are immutable DAG entries.
- **A fork's cost is its delta.** `forks/<id>/upper` starts empty on every
  fork *and* after every snapshot; history lives in the sealed layers.
- **The DAG is the filesystem.** `metadata/*.json` (`ParentID`) is the edge
  list; `checkpoints/*/upper` are the nodes' payloads; a fork's `LayerIDs` is
  a root-to-node path through the DAG, applied left-to-right (oldest at the
  bottom) as overlay lowerdirs.
- **`work/` is a name, not a place.** After the first snapshot, nothing is
  mounted there in the host namespace; each fork mounts its own overlay at
  that canonical path privately, so CRIU images (which bake in absolute
  paths) stay valid for every fork.

One refinement when `tmpfs_images` is enabled: `checkpoints/<ckpt>/criu` is a
*symlink*, first to a tmpfs dir the dump writes into (fast), then — after a
background flusher copies the images — atomically repointed at
`checkpoints/<ckpt>/criu.disk` (durable). Restores follow the symlink either
way; see `imagestore.go`.

## Three flows end to end

### `waypoint init <rootfs> --shell`

```
CLI (main.go)
  -> NewManagerWithSession                 (manager.go)
       mint session ID
       scaffold <sessions>/<session>/ + global session registry
  -> Manager.InitEnvironment(rootfs)        (overlay.go)
       create forks/main/{upper,work,temp}
       mount overlay at <session>/work:
         lowerdir = <original rootfs>
         upperdir = forks/main/upper
         workdir  = forks/main/work
       mount runtime pseudo-fs under the merged root (/proc, /sys)
  -> Manager.StartShell(<session>/work)     (build.go)
       copy host bash_init -> <session>/work/.waypoint/bash_init
       stage bash_init runtime deps if dynamic
       verify /bin/bash exists; stage bash's runtime deps when possible
       compute canonical socket:
         <session>/temp/shell_<session>.sock
       start host bash_init in NEWPID|NEWNS|NEWNET|NEWIPC namespaces:
         bash_init <canonical-socket> <session>/work
         WAYPOINT_NAMESPACED=1
         WAYPOINT_REEXEC_PATH=/.waypoint/bash_init
       wait for /proc/<pid>/root/<canonical-socket> to become dialable
       save forks/main/fork.json + session shell PID/socket
```

The important bootstrap happens inside `bash_init` before it starts bash:

```
bash_init first image (loaded from the host)
  setupNamespaceRuntime(<session>/work)       (cmd/bash-init/main.go)
    make mounts private
    bind-mount <session>/work onto itself     (pivot_root requires a mount)
    pivot_root(<session>/work, .waypoint-old-root)
    chdir("/")
    lazy-unmount /.waypoint-old-root
    assemble /dev inside the session root     (mountDeviceRuntime; OCI-style)
      mknod any missing basic char devices    (null, zero, full, random,
                                               urandom, tty)
      symlink fd/stdin/stdout/stderr          -> /proc/self/fd[/N]
      mount a private tmpfs on /dev/shm
      mount a private devpts on /dev/pts      (newinstance, ptmxmode=0666)
      symlink /dev/ptmx -> pts/ptmx
    mount /proc, /sys
    bring loopback up
  execve("/.waypoint/bash_init", socket, "/")

bash_init second image (loaded from inside the overlay)
  open PTY master/slave, set a sane terminal mode + size
  create /.waypoint/exec.done FIFO for out-of-band exit codes
  start /bin/bash on the PTY slave as its controlling terminal
  release bash's pidfd so CRIU does not see an undumpable anon inode
  prove bash can run commands, then listen on the Unix socket
  detach stdio to namespace /dev/null so host log fds are not checkpointed
```

With the old model, `bash_init` ran from the host and only its child bash was 
chrooted, so CRIU recorded host executable paths and other host-root references. 
Now the checkpointed supervisor is `/.waypoint/bash_init` inside the overlay, 
with bash, PTY devices, the completion FIFO, and the control socket all in the 
same session filesystem view. Every later fork can provide that same internal 
view in its own mount namespace.

`/dev` is assembled the way OCI runtimes (runc et al.) do it — device nodes,
symlinks, and per-session `devpts`/`tmpfs` mounts created inside the session's
own CoW root — with one deliberate difference: the nodes live in the overlay
itself (seeded by `prepareDevNodes` at build time, completed here) rather than
on a tmpfs, so fork restores get them from the overlay lowers with no extra
mount. **Never mount `devtmpfs` here.** It is a kernel-wide singleton
superblock shared with the host's `/dev`; a private mount namespace isolates
the mount table, not file contents, so mutations like the `/dev/ptmx` symlink
swap would propagate to the host and break `forkpty(3)` for every unprivileged
host process until reboot. `scripts/demo.sh` asserts host `/dev/ptmx` integrity
after every run to keep this invariant honest.

### `waypoint fork <session> A --id f1`

```
CLI (main.go)
  -> Manager.ForkCheckpoint            (checkpoint.go)
       LoadCheckpoint A                (metadata + paths)
       CRIUMaterializer.Materialize:
         withSessionLock: newForkRecord + mkdir upper/work/temp + save fork.json
         withForkLock:
           runRestoreHelper -> re-exec `__waypoint_restore_fork_child`
             new NEWNS|NEWNET|NEWIPC namespaces          (restoreForkChild)
             mount f1's overlay at <session>/work        (mountOverlay)
             bring loopback up
             criu restore --restore-detached --pidfile   (CRIU rebuilds pidns)
           read restored PID, rewrite socket to /proc/<pid>/root/...
           dial socket until ready -> status = running
```

### `waypoint exec <session> f1 -- 'echo hi'`

```
CLI (main.go)
  -> Manager.ExecuteForkCommand        (fork.go)
       withForkLock (serializes same-fork commands):
         load fork.json, check status == running
         execCommand(socket, "echo hi\n")               (fork.go)
           -> bash_init handleClient                    (cmd/bash-init)
                inject command into PTY + a completion printf to the FIFO
                read exit code off /.waypoint/exec.done
                reply "WP2 ok 0\n<output>"
       print output, exit with the command's code        (printExecResult)
```

Different forks take different fork locks, so they run concurrently; commands
on the same fork serialize on its lock.
