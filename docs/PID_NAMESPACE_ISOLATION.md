# PID-namespace isolation — the fix for CRIU restore failures (P0 + P1)

_Investigation 2026-07-01 on branch `feature/session-isolation`._

## STATUS: IMPLEMENTED & VALIDATED (2026-07-01)

Implemented as:
- `build.go StartShell`: `SysProcAttr.Cloneflags = syscall.CLONE_NEWPID` (session runs as PID 1 of a private PID namespace; networking stays on the host ns so apt/pip/uv keep internet).
- `memory.go restoreMemoryState`: `--pidfile` captures the restored root's new **host** PID (it changes on every restore into a fresh namespace) and returns it.
- `manager.go CreateCheckpointNew` / `RestoreCheckpointNew`: dropped the host-PID conflict cleanup (`prepareCheckpointRestore`) — a fresh namespace has no host conflicts; restore now just kills the *live* session to release the overlay. Both re-track the new host PID via `saveSessionInfo` so later dump/exec/kill target it.

Validated (harness `validate_reliability.py`, concurrency 1):
- **filter-js-from-html (RESTORE) → RELIABLE**; **mailman, hf-model-inference (SNAPSHOT) → RELIABLE**. `restore(s0)` now succeeds (`Restore finished successfully`).
- **pypi-server**: snapshot failure fixed (no longer SNAPSHOT); residual verify/EXEC failure remains (service reachability — likely needs the later `CLONE_NEWNET` stage).
- **No regression**: openssl, sqlite-db-truncate, kv-store-grpc (setsid daemon), write-compressor all still RELIABLE. **0 leaked daemons** post-run (the PID namespace reaps the tree on teardown — also fixes the daemon-leak/contamination problem).

Net: 3 of the 4 target failures fully fixed + the daemon-leak isolation problem solved. Remaining: pypi-server's service reachability, the `CLONE_NEWNET` concurrency stage, and the non-CRIU EXEC failures (git/terminal/qemu). Follow-ups below.

---

## Root cause (validated)

Waypoint build-mode runs the chroot session in the **host PID namespace**. CRIU
restore recreates a checkpoint's processes with their **original PIDs**. In a
shared namespace those PIDs are frequently still occupied (by the continuously
running session, by task daemons, or by PID reuse), so restore fails:

```
Forking task with 2765667 pid (flags 0x0)
Error (criu/cr-restore.c:1230): Can't fork for 2765667: File exists
Restoring FAILED.
```

`prepareCheckpointRestore` already tries to kill conflicting PIDs and waits 5 s,
but a shared namespace can **never** guarantee a specific PID stays free — the
restore `fork()` races against the kernel's global PID allocator.

## This one bug causes 4 of the 11 reliability failures

Both the restore path *and* the snapshot path hit it (snapshot =
`CreateCheckpointNew` dumps, then **re-restores** the process into the new
overlay to keep it running):

| task | reported bucket | real failure |
|---|---|---|
| filter-js-from-html | RESTORE-FIDELITY | `restore(s0)` → CRIU fork/PID conflict |
| pypi-server | WAYPOINT-SNAPSHOT | snapshot's re-restore → same PID conflict |
| mailman | WAYPOINT-SNAPSHOT | same |
| hf-model-inference | WAYPOINT-SNAPSHOT | same |

(The 3 qemu/VM EXEC failures and the git/terminal/log EXEC failures are
unrelated — those are exec-model / nested-virt issues, not CRIU.)

## Fix: private PID namespace per session

Run the session (`bash_init` + every task process) in its own PID namespace so
checkpointed PIDs are namespace-local and always free on restore. This also
makes teardown reap the whole tree by killing the namespace init, and is the
foundation for safe concurrency (P1).

### Why it's not a one-liner (validated experiment)

Adding `SysProcAttr.Cloneflags = syscall.CLONE_NEWPID` to `StartShell` (build.go)
**did** make CRIU capture namespace-local PIDs (1..6) — the desired effect — but
broke the snapshot path, because the host-PID-based bookkeeping now misfires:

```
failed to prepare memory restore into new overlay:
checkpoint task IDs still exist after cleanup: 1, 2, 3, 4, 5, 6
```

`findTaskOwnerPID` / `findConflictingCheckpointTasks` (process.go) look up ns
PIDs 1..6 in the **host** namespace, where they're always taken (init +
kthreads). So the change was reverted; the real fix is a namespace-aware rework:

### Implementation plan

1. **Launch** `bash_init` with `CLONE_NEWPID | CLONE_NEWNS` (build.go `StartShell`).
   Set mount propagation to private and mount a fresh `/proc` inside the ns so
   `/proc` reflects the namespace (required for CRIU and for `ps`/tools).
   `bash_init` becomes PID 1 → must reap children (add a `wait4` loop) and handle
   signals as init.
2. **CRIU dump** (`memory.go`): dump the namespace init; verify CRIU records the
   pidns (it does when `-t` is the ns init). Likely no flag change beyond what's
   there.
3. **CRIU restore**: CRIU recreates the pidns on restore. **Remove/guard the
   host-PID conflict logic** (`prepareCheckpointRestore`, `findConflictingCheckpointTasks`)
   for the namespaced case — there are no host conflicts by construction. This is
   the crux of the rework.
4. **exec** goes through the shell socket, so it already runs inside the ns — no
   change. (A future direct-exec path would need `setns`.)
5. **Teardown**: kill the ns init (reaps all); existing overlay/mount cleanup
   under `WAYPOINT_SESSIONS_DIR` unchanged. This is *safer* than today (no leaked
   daemons), but re-review unmount ordering (the June-30 wipe was in teardown).
6. **Validate** against filter-js / pypi-server / mailman / hf-model-inference
   (should flip to RELIABLE) and re-run the 71 currently-reliable tasks to ensure
   no regression (especially `/proc`-reading tools and network — network stays on
   the host namespace in this stage, so apt/pip/uv keep working).

### Later (P1 concurrency): add `CLONE_NEWNET`

A private network namespace gives per-session ports (fixes the service-task port
collisions and enables safe parallelism) — but needs veth+NAT or slirp4netns so
tasks keep internet access (apt/pip/uv). Separable from, and after, the PID-ns work.
