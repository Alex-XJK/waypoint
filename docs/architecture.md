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
  onto it — this is what makes recursive forking ordinary.

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
| `cmd/waypoint` | The CLI. Thin argv dispatch over the `waypoint` package. Also hosts the hidden `__waypoint_restore_fork_child` re-exec entry point. |
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
- `config.go` — configuration loading (`loadConfig`: env/file overrides for
  sessions dir and bash_init path, each tracked back to its source) and
  `LoadConfigInfo` / `ConfigInfo` for the `info` command.
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
- `cleanup.go` — the three `Cleanup*` variants (graceful, forced, interactive),
  process existence/kill helpers, and the force-cleanup crew
  (`lsof`/`fuser`/`findmnt`-based).
- `build.go` — `StartShell` (stage `bash_init` into the overlay and launch it
  namespaced), `BuildEnvironment` / `BuildFromDockerfile` (buildah-based rootfs
  build), and dependency-staging helpers (`ldd` -> copy libs into rootfs).

## On-disk layout

```
/tmp/waypoint-sessions/<session>/
  work/                     canonical merged mountpoint (mounted per-fork,
                              differently, in each fork's mount namespace)
  metadata/<ckpt>.json      checkpoint DAG nodes
  checkpoints/<ckpt>/
    upper/                  sealed, immutable filesystem delta
    criu/                   CRIU memory image (+ dump.log)
  forks/<fork>/
    fork.json               live fork record
    upper/  work/           this fork's private CoW layers
    lock                    per-fork flock
    restore.log restore.pid
  locks/session.lock        session-wide flock

/tmp/waypoint-sessions-info/<session>.json   global registry (find a session)
```

Overlay lowerdir order for a fork on checkpoint chain `[A,B,C]`:

```
lowerdir = C/upper : B/upper : A/upper : <original rootfs>   (highest prio left)
upperdir = forks/<fork>/upper      (private, writable)
```

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
       EnsureCriuCompatible                 (criu.go)
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
    mount /dev, /dev/pts, /proc, /sys
    make /dev/ptmx -> /dev/pts/ptmx
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

### `waypoint fork <session> A --id f1`

```
CLI (main.go)
  -> Manager.ForkCheckpoint            (checkpoint.go)
       EnsureCriuCompatible            (criu.go)
       LoadCheckpoint A                (metadata + paths)
       CRIUMaterializer.Materialize:
         withSessionLock: newForkRecord + mkdir upper/work + save fork.json
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
