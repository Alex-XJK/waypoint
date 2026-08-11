#!/bin/bash
# End-to-end demo and functional test of Waypoint parallel forking.
#
# Walks every user-facing operation (init/checkpoint/fork/exec/snapshot/
# snapshot --park/destroy/list/info/cleanup) and asserts the semantics the
# fork model promises: delta layers, whiteout correctness across snapshot
# chains, divergence isolation, park/revive round-trips, error paths, and
# no leaked mounts or processes after cleanup. Exits non-zero if any
# assertion fails.
#
# Requires (see the "Environment" checks below):
#   - root (CRIU needs it)
#   - CRIU >= 4.0  (older CRIU cannot restore PAC-built binaries on arm64)
#   - Go toolchain
#
# Usage:  sudo ./scripts/demo.sh
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d /tmp/waypoint-demo.XXXXXX)"
ROOTFS="$WORK/rootfs"
BIN="$WORK/bin"
GOCACHE="${GOCACHE:-/tmp/waypoint-go-cache}"
SESSION=""
SESSIONS_DIR="${WAYPOINT_SESSIONS_DIR:-/tmp/waypoint-sessions}"

say()  { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
run()  { printf '\033[0;90m$ %s\033[0m\n' "$*"; eval "$*"; }

PASS=0; FAIL=0
HOST_HOSTNAME="$(cat /proc/sys/kernel/hostname)"
ok()   { PASS=$((PASS+1)); printf '   \033[0;32mok\033[0m  %s\n' "$*"; }
bad()  { FAIL=$((FAIL+1)); printf '   \033[0;31mFAIL\033[0m %s\n' "$*"; }

# assert_contains <desc> <needle> <haystack>
assert_contains() {
  case "$3" in *"$2"*) ok "$1";; *) bad "$1 — expected output to contain '$2'; got: $(echo "$3" | head -3)";; esac
}
# assert_not_contains <desc> <needle> <haystack>
assert_not_contains() {
  case "$3" in *"$2"*) bad "$1 — output unexpectedly contains '$2'";; *) ok "$1";; esac
}
assert_exists()  { [ -e "$2" ] && ok "$1" || bad "$1 — missing: $2"; }
assert_absent()  { [ ! -e "$2" ] && ok "$1" || bad "$1 — should not exist: $2"; }
# assert_fails <desc> <cmd...>  — the command must exit non-zero
assert_fails() {
  local desc="$1"; shift
  if OUT="$("$@" 2>&1)"; then bad "$desc — expected failure, got success: $(echo "$OUT" | head -1)"
  else ok "$desc ($(echo "$OUT" | head -1))"; fi
}

cleanup() {
  [ -n "$SESSION" ] && "$BIN/waypoint" cleanup "$SESSION" --force >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

ck_upper() { echo "$SESSIONS_DIR/$SESSION/checkpoints/$1/upper"; }
ck_criu()  { echo "$SESSIONS_DIR/$SESSION/checkpoints/$1/criu"; }
fork_dir() { echo "$SESSIONS_DIR/$SESSION/forks/$1"; }

# ---------------------------------------------------------------------------
say "Environment checks"
# ---------------------------------------------------------------------------
[ "$(id -u)" -eq 0 ] || { echo "must run as root (CRIU needs it)"; exit 1; }

criu_ver="$(criu --version 2>/dev/null | grep -oE '[0-9]+\.[0-9]+' | head -1)"
echo "criu: ${criu_ver:-not found}  ($(command -v criu))"
major="${criu_ver%%.*}"
if [ "$(uname -m)" = "aarch64" ] && grep -qwE 'paca|pacg' /proc/cpuinfo; then
  echo "host has ARM64 pointer authentication -> CRIU >= 4.0 is REQUIRED"
  [ "${major:-0}" -ge 4 ] || { echo "FATAL: criu $criu_ver too old; build >= 4.0"; exit 1; }
fi
criu check >/dev/null 2>&1 && echo "criu check: ok" || echo "criu check: WARNING (some features unavailable)"

# ---------------------------------------------------------------------------
say "Build waypoint + bash_init"
# ---------------------------------------------------------------------------
# sudo often has a trimmed PATH; find the Go toolchain explicitly.
GO="$(command -v go || true)"
for cand in /usr/local/go/bin/go "$HOME/go/bin/go" "/home/$SUDO_USER/go/bin/go"; do
  [ -n "$GO" ] && break
  [ -x "$cand" ] && GO="$cand"
done
[ -n "$GO" ] || { echo "go toolchain not found on PATH; set it or run: PATH=/usr/local/go/bin:\$PATH sudo -E ./scripts/demo.sh"; exit 1; }
mkdir -p "$BIN"
( cd "$REPO"
  env GOCACHE="$GOCACHE" "$GO" build -o "$BIN/waypoint" ./cmd/waypoint
  # bash_init is staged inside the container; build it static so it needs no
  # host loader/libs inside the rootfs.
  env GOCACHE="$GOCACHE" CGO_ENABLED=0 "$GO" build -o "$BIN/bash_init" ./cmd/bash-init )
export WAYPOINT_BASH_INIT_SRC="$BIN/bash_init"
echo "built into $BIN"

# ---------------------------------------------------------------------------
say "Assemble a minimal rootfs"
# ---------------------------------------------------------------------------
# No /dev seeding needed: bash_init assembles the sandbox /dev at session
# start. Commands go in /bin, which the fixed guest PATH includes.
mkdir -p "$ROOTFS"/{bin,tmp,proc,sys,root}
for b in bash cat ls sleep mkdir rm ps grep touch; do
  p="$(command -v "$b" || true)"
  [ -n "$p" ] && [ -f "$p" ] && cp "$p" "$ROOTFS/bin/$(basename "$b")"
done
# copy every shared-lib dependency, preserving its absolute path
for b in "$ROOTFS"/bin/*; do
  ldd "$b" 2>/dev/null | grep -oE '/[^ ]+\.so[^ ]*' | while read -r lib; do
    [ -f "$lib" ] || continue
    mkdir -p "$ROOTFS$(dirname "$lib")"
    cp -n "$(readlink -f "$lib")" "$ROOTFS$lib" 2>/dev/null || true
  done
done
# dynamic loader
for l in /lib/ld-linux-aarch64.so.1 /lib64/ld-linux-x86-64.so.2; do
  [ -f "$l" ] && { mkdir -p "$ROOTFS$(dirname "$l")"; cp -n "$l" "$ROOTFS$l"; }
done
echo "rootfs: $ROOTFS ($(du -sh "$ROOTFS" | cut -f1))"

W="$BIN/waypoint"
SLEEPS_BEFORE="$(pgrep -fc 'sleep 600' || true)"

# ---------------------------------------------------------------------------
say "1. init a session with a live shell (the 'main' fork)"
# ---------------------------------------------------------------------------
OUT="$("$W" init "$ROOTFS" --shell --quiet)"
SESSION="${OUT%%,*}"
echo "session: $SESSION  (dir: $SESSIONS_DIR/$SESSION)"
assert_exists "session dir created" "$SESSIONS_DIR/$SESSION"

# ---------------------------------------------------------------------------
say "2. build hidden shell state + filesystem state in main"
# ---------------------------------------------------------------------------
run "$W exec $SESSION main -- 'cd /root; GREETING=hello-from-checkpoint; sleep 600 & echo started job \$!'"
run "$W exec $SESSION main -- 'echo base-content > /root/base.txt'"
assert_exists "main's write lands in its upper (CoW, not the rootfs)" "$(fork_dir main)/upper/root/base.txt"
assert_absent "the original rootfs is untouched" "$ROOTFS/root/base.txt"

# The guest environment is the fixed set from StartShell, not the invoking
# user's (host env would be baked into every checkpoint of the session).
OUT="$("$W" exec "$SESSION" main -- 'echo "PATH=$PATH"; export -p' 2>&1)"
assert_contains     "guest PATH is the fixed default" "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" "$OUT"
assert_not_contains "host env does not leak into the guest" "SUDO_USER" "$OUT"
assert_not_contains "waypoint plumbing vars are stripped from the guest" "WAYPOINT_" "$OUT"

# UTS namespace: the sandbox has its own hostname.
OUT="$("$W" exec "$SESSION" main -- 'cat /proc/sys/kernel/hostname' 2>&1)"
assert_contains "guest has its own hostname (UTS namespace)" "waypoint" "$OUT"

# ---------------------------------------------------------------------------
say "3. checkpoint main -> immutable checkpoint A (delta seal)"
# ---------------------------------------------------------------------------
OUT="$("$W" checkpoint "$SESSION" A 2>&1)"; echo "$OUT"
assert_contains     "checkpoint A created" "created successfully" "$OUT"
assert_not_contains "phase stats are off by default" "phases" "$OUT"
assert_exists "A's layer holds the sealed delta" "$(ck_upper A)/root/base.txt"
assert_exists "A has CRIU images" "$(ck_criu A)"
assert_absent "main's upper was reset by the seal (rename, not copy)" "$(fork_dir main)/upper/root/base.txt"

# ---------------------------------------------------------------------------
say "4. hidden state survives the destructive dump + restore of main"
# ---------------------------------------------------------------------------
OUT="$("$W" exec "$SESSION" main -- 'echo cwd=$(pwd) GREETING=$GREETING; jobs -r' 2>&1)"; echo "$OUT"
assert_contains "cwd survived"        "cwd=/root" "$OUT"
assert_contains "shell var survived"  "GREETING=hello-from-checkpoint" "$OUT"
assert_contains "background job survived" "sleep" "$OUT"
OUT="$("$W" exec "$SESSION" main -- 'cat /root/base.txt' 2>&1)"
assert_contains "main still sees its file through the new layer" "base-content" "$OUT"

# ---------------------------------------------------------------------------
say "5. fork A into independent live shells (sequential + concurrent + --n)"
# ---------------------------------------------------------------------------
run "$W fork $SESSION A --id f1"
run "$W fork $SESSION A --id f2"
"$W" fork "$SESSION" A --id c1 >/dev/null 2>&1 &
"$W" fork "$SESSION" A --id c2 >/dev/null 2>&1 &
wait
OUT="$("$W" exec "$SESSION" c1 -- 'echo c1-alive' 2>&1)"
assert_contains "concurrent fork c1 is alive" "c1-alive" "$OUT"
OUT="$("$W" exec "$SESSION" c2 -- 'echo c2-alive' 2>&1)"
assert_contains "concurrent fork c2 is alive" "c2-alive" "$OUT"
OUT="$("$W" fork "$SESSION" A --n 2 2>&1)"; echo "$OUT"
N="$(echo "$OUT" | grep -c 'pid=' || true)"
[ "$N" -eq 2 ] && ok "fork --n 2 materialized 2 forks" || bad "fork --n 2: expected 2 forks, saw $N"

# ---------------------------------------------------------------------------
say "6. forks inherit state, then diverge without touching each other"
# ---------------------------------------------------------------------------
OUT="$("$W" exec "$SESSION" f1 -- 'echo f1 GREETING=$GREETING cwd=$(pwd); cat /root/base.txt' 2>&1)"; echo "$OUT"
assert_contains "f1 inherited shell state" "GREETING=hello-from-checkpoint" "$OUT"
assert_contains "f1 inherited fs state"    "base-content" "$OUT"
run "$W exec $SESSION f2 -- 'GREETING=f2-only; cd /tmp; echo f2-file > /root/f2.txt'"
OUT="$("$W" exec "$SESSION" f1 -- 'echo GREETING=$GREETING cwd=$(pwd); ls /root' 2>&1)"; echo "$OUT"
assert_contains     "f1's shell state is untouched by f2" "GREETING=hello-from-checkpoint" "$OUT"
assert_not_contains "f1 does not see f2's file"           "f2.txt" "$OUT"
OUT="$("$W" exec "$SESSION" f2 -- 'echo GREETING=$GREETING cwd=$(pwd)' 2>&1)"; echo "$OUT"
assert_contains "f2 kept its own divergence" "GREETING=f2-only" "$OUT"
assert_contains "f2 kept its own cwd"        "cwd=/tmp" "$OUT"
OUT="$("$W" exec "$SESSION" f1 -- 'cat /proc/sys/kernel/hostname' 2>&1)"
assert_contains "fork keeps the sandbox hostname (CRIU restores the UTS ns)" "waypoint" "$OUT"

# ---------------------------------------------------------------------------
say "7. exit codes propagate"
# ---------------------------------------------------------------------------
if "$W" exec "$SESSION" f1 -- 'ls /does-not-exist' >/dev/null 2>&1; then
  bad "exec of a failing command should exit non-zero"
else
  ok "exec propagated the command's non-zero exit code ($?)"
fi

# ---------------------------------------------------------------------------
say "8. snapshot seals a true delta layer (not a cumulative copy)"
# ---------------------------------------------------------------------------
run "$W snapshot $SESSION f2 B"
assert_exists "B's layer has f2's delta"          "$(ck_upper B)/root/f2.txt"
assert_absent "B's layer does NOT re-seal base.txt (true delta)" "$(ck_upper B)/root/base.txt"
run "$W fork $SESSION B --id f3"
OUT="$("$W" exec "$SESSION" f3 -- 'echo GREETING=$GREETING; cat /root/base.txt /root/f2.txt' 2>&1)"; echo "$OUT"
assert_contains "f3 inherited f2's diverged shell state" "GREETING=f2-only" "$OUT"
assert_contains "f3 sees the full layered fs (A + B)" "base-content" "$OUT"
assert_contains "f3 sees f2's file"                   "f2-file" "$OUT"

# ---------------------------------------------------------------------------
say "9. whiteout correctness: deletions must not resurrect across chains"
# ---------------------------------------------------------------------------
# The leave-running model lost this: a file created after fork creation,
# sealed into a checkpoint, then deleted, reappeared in later forks because
# the live mount never produced a whiteout for it.
run "$W exec $SESSION f3 -- 'touch /root/ghost.txt'"
run "$W snapshot $SESSION f3 C1"
run "$W exec $SESSION f3 -- 'rm /root/ghost.txt /root/base.txt'"
run "$W snapshot $SESSION f3 C2"
WH="$(find "$(ck_upper C2)" -type c 2>/dev/null | wc -l)"
[ "$WH" -ge 2 ] && ok "C2's layer carries whiteouts for both deletions" || bad "C2's layer has $WH whiteout(s), expected 2 (ghost.txt + base.txt)"
run "$W fork $SESSION C2 --id f4"
OUT="$("$W" exec "$SESSION" f4 -- 'ls /root' 2>&1)"; echo "$OUT"
assert_not_contains "deleted ghost.txt did not resurrect in f4" "ghost.txt" "$OUT"
assert_not_contains "deleted base.txt did not resurrect in f4"  "base.txt" "$OUT"
assert_contains     "f2.txt (untouched) is still visible"       "f2.txt" "$OUT"

# ---------------------------------------------------------------------------
say "10. park: persist a fork to disk without resuming it, then revive"
# ---------------------------------------------------------------------------
run "$W fork $SESSION A --id p1"
run "$W exec $SESSION p1 -- 'cd /root; PARKVAR=survived-the-park; echo parked-content > /root/parked.txt'"
OUT="$("$W" snapshot "$SESSION" p1 P --park 2>&1)"; echo "$OUT"
assert_contains "park reported success" "parked as checkpoint" "$OUT"
assert_absent   "parked fork's runtime dir is gone" "$(fork_dir p1)"
assert_exists   "P's layer has the parked delta"    "$(ck_upper P)/root/parked.txt"
assert_exists   "P has CRIU images"                 "$(ck_criu P)"
OUT="$("$W" list "$SESSION" --json 2>&1)"
# checkpoint P legitimately records created_from_fork_id="p1"; only the live
# forks section must be free of the parked fork
FORKS_OUT="$(printf '%s\n' "$OUT" | sed -n '/"forks"/,$p')"
assert_not_contains "parked fork no longer listed" '"p1"' "$FORKS_OUT"
assert_contains     "parked checkpoint is listed"  '"P"'  "$OUT"
# revive under the same (now free) fork ID
run "$W fork $SESSION P --id p1"
OUT="$("$W" exec "$SESSION" p1 -- 'echo PARKVAR=$PARKVAR cwd=$(pwd); cat /root/parked.txt' 2>&1)"; echo "$OUT"
assert_contains "revived fork kept its shell var" "PARKVAR=survived-the-park" "$OUT"
assert_contains "revived fork kept its cwd"       "cwd=/root" "$OUT"
assert_contains "revived fork kept its fs delta"  "parked-content" "$OUT"
assert_fails "parking main is refused" "$W" snapshot "$SESSION" main M --park

# ---------------------------------------------------------------------------
say "11. destroy: discard a fork (checkpoints unaffected)"
# ---------------------------------------------------------------------------
run "$W fork $SESSION A --id d1"
run "$W exec $SESSION d1 -- 'echo doomed > /root/doomed.txt'"
DESTROY_START_MS=$(( $(date +%s%N) / 1000000 ))
run "$W destroy $SESSION d1"
DESTROY_MS=$(( $(date +%s%N) / 1000000 - DESTROY_START_MS ))
if [ "$DESTROY_MS" -lt 2000 ]; then
  ok "destroy completes without a kill-grace stall (${DESTROY_MS}ms)"
else
  bad "destroy took ${DESTROY_MS}ms — kill-grace stall is back?"
fi
assert_absent "destroyed fork's dir is gone" "$(fork_dir d1)"
assert_fails  "exec on a destroyed fork fails" "$W" exec "$SESSION" d1 -- 'echo zombie'
assert_exists "checkpoint A is unaffected" "$(ck_upper A)/root/base.txt"

# ---------------------------------------------------------------------------
say "12. checkpoint chain built by park (fork -> write -> park) x5"
# ---------------------------------------------------------------------------
TIP=A
for i in 1 2 3 4 5; do
  "$W" fork "$SESSION" "$TIP" --id chain >/dev/null
  "$W" exec "$SESSION" chain -- "echo level-$i > /root/chain$i.txt" >/dev/null
  "$W" snapshot "$SESSION" chain "D$i" --park >/dev/null
  TIP="D$i"
done
echo "built chain A -> D1 -> ... -> D5 (fork ID 'chain' reused at every level)"
run "$W fork $SESSION D5 --id leaf"
OUT="$("$W" exec "$SESSION" leaf -- 'ls /root' 2>&1)"; echo "$OUT"
for i in 1 2 3 4 5; do
  assert_contains "leaf sees level $i of the chain" "chain$i.txt" "$OUT"
done
assert_exists "each chain level sealed only its own delta" "$(ck_upper D3)/root/chain3.txt"
assert_absent "…and nothing from other levels"             "$(ck_upper D3)/root/chain2.txt"

# ---------------------------------------------------------------------------
say "13. concurrency: two 2s commands on different forks finish in ~2s"
# ---------------------------------------------------------------------------
start=$(date +%s)
"$W" exec "$SESSION" f1 -- 'sleep 2; echo f1 done' &
"$W" exec "$SESSION" f3 -- 'sleep 2; echo f3 done' &
wait
elapsed=$(( $(date +%s) - start ))
echo "  -> wall clock: ${elapsed}s"
[ "$elapsed" -le 3 ] && ok "parallel exec did not serialize (${elapsed}s)" || bad "parallel exec took ${elapsed}s (>3s: forks may be serializing)"

# ---------------------------------------------------------------------------
say "14. phase stats appear only when WAYPOINT_PHASE_STATS=1"
# ---------------------------------------------------------------------------
OUT="$(WAYPOINT_PHASE_STATS=1 "$W" fork "$SESSION" A --id ps1 2>&1)"; echo "$OUT"
assert_contains "fork prints a restore breakdown when enabled" "_ms=" "$OUT"
OUT="$(WAYPOINT_PHASE_STATS=1 "$W" snapshot "$SESSION" ps1 PS 2>&1)"; echo "$OUT"
assert_contains "snapshot prints phases when enabled" "phases" "$OUT"
OUT="$("$W" snapshot "$SESSION" ps1 PS2 2>&1)"
assert_not_contains "snapshot is quiet when disabled" "phases" "$OUT"

# ---------------------------------------------------------------------------
say "15. tmpfs images (WAYPOINT_TMPFS_IMAGES=1): dump to RAM, flush to disk"
# ---------------------------------------------------------------------------
if [ -d /dev/shm ]; then
  OUT="$(WAYPOINT_TMPFS_IMAGES=1 "$W" snapshot "$SESSION" f1 T 2>&1)"; echo "$OUT"
  assert_contains "tmpfs snapshot succeeded" "snapshotted as checkpoint" "$OUT"
  [ -L "$(ck_criu T)" ] && ok "checkpoint criu path is a symlink" || bad "expected $(ck_criu T) to be a symlink"
  OUT="$(WAYPOINT_TMPFS_IMAGES=1 "$W" fork "$SESSION" T --id t1 2>&1)"
  assert_contains "fork from a tmpfs-image checkpoint works" "pid=" "$OUT"
  flushed=""
  for _ in $(seq 1 30); do
    [ -d "$SESSIONS_DIR/$SESSION/checkpoints/T/criu.disk" ] && { flushed=1; break; }
    sleep 0.5
  done
  [ -n "$flushed" ] && ok "async flusher persisted the images to disk" || bad "criu.disk did not appear within 15s (flusher stuck?)"
else
  echo "  (skipped: /dev/shm not available)"
fi

# ---------------------------------------------------------------------------
say "16. error paths reject cleanly"
# ---------------------------------------------------------------------------
assert_fails "duplicate checkpoint ID"        "$W" checkpoint "$SESSION" A
assert_fails "duplicate live fork ID"         "$W" fork "$SESSION" A --id f1
assert_fails "fork from missing checkpoint"   "$W" fork "$SESSION" NOPE --id x1
assert_fails "snapshot of missing fork"       "$W" snapshot "$SESSION" nope X1
assert_fails "reserved checkpoint ID"         "$W" snapshot "$SESSION" f1 current

# ---------------------------------------------------------------------------
say "17. inspect the DAG + info"
# ---------------------------------------------------------------------------
run "$W list $SESSION --json | grep -E '\"id\"|\"status\"|\"base_checkpoint_id\"' | head -30"
OUT="$("$W" info "$SESSION" 2>&1)"
assert_contains "info shows the session" "$SESSION" "$OUT"
OUT="$("$W" info "$SESSION" D3 2>&1)"
assert_contains "checkpoint info records DAG lineage (D3's parent is D2)" '"parent_id": "D2"' "$OUT"

# ---------------------------------------------------------------------------
say "18. suspend: end all compute, keep the DAG on disk"
# ---------------------------------------------------------------------------
run "$W suspend $SESSION"
MNTS="$(grep -c "$SESSION" /proc/mounts || true)"
[ "$MNTS" -eq 0 ] && ok "no mounts after suspend" || bad "$MNTS mount(s) survived suspend"
assert_absent "running fork destroyed by suspend" "$(fork_dir c1)"
assert_fails  "exec on a suspended session's fork fails" "$W" exec "$SESSION" c1 -- 'echo zombie'
assert_exists "checkpoint layers survive suspend" "$(ck_upper A)/root/base.txt"
assert_exists "session stays registered" "/tmp/waypoint-sessions-info/$SESSION.json"
CRIU_T="$(readlink "$(ck_criu T)" 2>/dev/null || echo not-a-symlink)"
case "$CRIU_T" in
  /dev/shm*) bad "checkpoint T images still on tmpfs after suspend ($CRIU_T)";;
  *)         ok  "tmpfs images flushed to durable disk (T -> $CRIU_T)";;
esac
OUT="$("$W" fork "$SESSION" A --id woke 2>&1)"
assert_contains "fork from a checkpoint resumes the suspended session" "pid=" "$OUT"
OUT="$("$W" exec "$SESSION" woke -- 'echo GREETING=$GREETING; cat /root/base.txt' 2>&1)"
assert_contains "resumed fork restored shell state"      "GREETING=hello-from-checkpoint" "$OUT"
assert_contains "resumed fork restored filesystem state" "base-content" "$OUT"

# Recursive forking keeps working after a suspend: diverge the resumed fork,
# seal it, and fork the new checkpoint.
run "$W exec $SESSION woke -- 'echo post-suspend > /root/woke.txt'"
OUT="$("$W" snapshot "$SESSION" woke W1 2>&1)"
assert_contains "resumed fork snapshots into a new checkpoint" "snapshotted as checkpoint" "$OUT"
OUT="$("$W" fork "$SESSION" W1 --id woke2 2>&1)"
assert_contains "fork of the post-suspend checkpoint works" "pid=" "$OUT"
OUT="$("$W" exec "$SESSION" woke2 -- 'cat /root/woke.txt /root/base.txt; echo GREETING=$GREETING' 2>&1)"
assert_contains "grandchild sees the post-suspend divergence" "post-suspend" "$OUT"
assert_contains "grandchild sees the pre-suspend layer chain" "base-content" "$OUT"
assert_contains "grandchild inherited shell state through both hops" "GREETING=hello-from-checkpoint" "$OUT"

# ---------------------------------------------------------------------------
say "19. cleanup leaves nothing behind"
# ---------------------------------------------------------------------------
run "$W cleanup $SESSION --force"
MNTS="$(grep -c "$SESSION" /proc/mounts || true)"
[ "$MNTS" -eq 0 ] && ok "no leftover mounts" || { bad "$MNTS mount(s) still reference the session"; grep "$SESSION" /proc/mounts || true; }
assert_absent "session dir removed" "$SESSIONS_DIR/$SESSION"
SLEEPS_AFTER="$(pgrep -fc 'sleep 600' || true)"
[ "${SLEEPS_AFTER:-0}" -le "${SLEEPS_BEFORE:-0}" ] && ok "no leaked fork processes (sleep-600 count: ${SLEEPS_BEFORE:-0} -> ${SLEEPS_AFTER:-0})" \
  || bad "leaked processes: sleep-600 count went ${SLEEPS_BEFORE:-0} -> ${SLEEPS_AFTER:-0}"
SESSION=""  # already cleaned; skip the trap's cleanup

# ---------------------------------------------------------------------------
say "20. host /dev is untouched"
# ---------------------------------------------------------------------------
# Sessions build their /dev inside the private session root; nothing they do
# may mutate the host's. In particular /dev/ptmx must remain a real character
# device (5:2) — replacing it with a pts/ptmx symlink breaks forkpty(3) for
# every unprivileged host process until reboot.
if [ -c /dev/ptmx ] && [ ! -L /dev/ptmx ]; then
  ok "host /dev/ptmx is still a real character device"
else
  bad "host /dev/ptmx was clobbered: $(ls -l /dev/ptmx 2>&1)"
fi
if [ "$(cat /proc/sys/kernel/hostname)" = "$HOST_HOSTNAME" ]; then
  ok "host hostname unchanged"
else
  bad "host hostname changed: $HOST_HOSTNAME -> $(cat /proc/sys/kernel/hostname)"
fi

# ---------------------------------------------------------------------------
say "Summary"
# ---------------------------------------------------------------------------
printf '%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ] || exit 1
