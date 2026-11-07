#!/bin/bash
# Runtime test of the pid allocator after restore. Stock criu puts every task
# back under its old pid but leaves ns_last_pid low, so a restored tree hands
# out pids below the ones it holds. The criu Waypoint runs with must restore
# ns_last_pid itself (the waypoint branch of our criu fork, version
# 4.2.1.20261026 or later); Waypoint no longer corrects it.
#
# Raises main's pid counter, starts a daemon that keeps forking, then checks
# after checkpoint, fork, and a second-generation fork that new processes and
# the daemon's new children get pids above the daemon's.
#
# Requires root, criu, a static busybox, and Go unless WAYPOINT_BIN and
# BASH_INIT_BIN are given. Sessions, registry and rootfs live in a private temp
# dir. BURN sets how many pids main uses up first (default 300).
#
# Usage:  sudo ./scripts/test-ns-last-pid.sh
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d /tmp/wppid.XXXXXX)"
ROOTFS="$WORK/rootfs"
BIN="$WORK/bin"
BURN="${BURN:-300}"
SESSION=""
CURRENT_SECTION="startup"
COMPLETED=0
PASS=0; FAIL=0

export WAYPOINT_SESSIONS_DIR="$WORK/s" WAYPOINT_SESSION_INFO_DIR="$WORK/i"
export WAYPOINT_NETWORK=shared
export WAYPOINT_PRESERVE_SESSION_ON_CLEANUP=false

say()  { CURRENT_SECTION="$*"; printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
ok()   { PASS=$((PASS+1)); printf '   \033[0;32mok\033[0m  %s\n' "$*"; }
bad()  { FAIL=$((FAIL+1)); printf '   \033[0;31mFAIL\033[0m %s\n' "$*"; }
check() { local desc="$1"; shift; if "$@"; then ok "$desc"; else bad "$desc"; fi; }
assert_gt() { [ "$2" -gt "$3" ] 2>/dev/null && ok "$1 ($2 > $3)" || bad "$1 — got '$2', want > $3"; }

cleanup() {
  if [ "$COMPLETED" -ne 1 ]; then
    printf '\n\033[0;31m!! aborted during "%s"\033[0m\n' "$CURRENT_SECTION"
  fi
  [ -n "$SESSION" ] && "$W" cleanup "$SESSION" --force >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

wx() { "$W" exec "$SESSION" "$1" -- "$2"; }
# new_pid <fork>: the pid a freshly forked process gets.
new_pid() { wx "$1" 'sh -c "echo \$\$"'; }
# daemon_alive <fork>: the restored daemon still runs under its old pid.
daemon_alive() { [ "$(wx "$1" "kill -0 $DAEMON && echo yes" 2>/dev/null)" = yes ]; }
# lowest_child <fork>: the lowest pid among the daemon's current children.
lowest_child() {
  wx "$1" "sleep 1; cat /proc/$DAEMON/task/$DAEMON/children" | tr ' ' '\n' | grep -E '^[0-9]+$' | sort -n | head -n 1
}

# ---------------------------------------------------------------------------
say "Environment checks"
# ---------------------------------------------------------------------------
[ "$(id -u)" -eq 0 ] || { echo "must run as root"; exit 1; }
for c in criu busybox python3; do
  command -v "$c" >/dev/null || { echo "missing required command: $c"; exit 1; }
done
if ldd "$(command -v busybox)" >/dev/null 2>&1; then
  echo "busybox must be statically linked (package busybox-static)"; exit 1
fi

# ---------------------------------------------------------------------------
say "Build waypoint + bash_init"
# ---------------------------------------------------------------------------
if [ -n "${WAYPOINT_BIN:-}" ] && [ -n "${BASH_INIT_BIN:-}" ]; then
  W="$WAYPOINT_BIN"; export WAYPOINT_BASH_INIT_SRC="$BASH_INIT_BIN"
else
  GO="$(command -v go || true)"
  for cand in /usr/local/go/bin/go "$HOME/go/bin/go" "/home/${SUDO_USER:-}/go/bin/go"; do
    [ -n "$GO" ] && break
    [ -x "$cand" ] && GO="$cand"
  done
  [ -n "$GO" ] || { echo "go toolchain not found; set WAYPOINT_BIN and BASH_INIT_BIN"; exit 1; }
  mkdir -p "$BIN"
  ( cd "$REPO"
    env GOCACHE="${GOCACHE:-/tmp/waypoint-go-cache}" "$GO" build -o "$BIN/waypoint" ./cmd/waypoint
    env GOCACHE="${GOCACHE:-/tmp/waypoint-go-cache}" CGO_ENABLED=0 "$GO" build -o "$BIN/bash_init" ./cmd/bash-init )
  W="$BIN/waypoint"; export WAYPOINT_BASH_INIT_SRC="$BIN/bash_init"
fi
echo "waypoint: $W"

# ---------------------------------------------------------------------------
say "Assemble a busybox rootfs"
# ---------------------------------------------------------------------------
mkdir -p "$ROOTFS"/{bin,tmp,proc,sys,root,etc}
cp "$(command -v bash)" "$ROOTFS/bin/bash"
{ ldd "$ROOTFS/bin/bash" 2>/dev/null | grep -oE '/[^ ]+\.so[^ ]*' || true; } | while read -r lib; do
  [ -f "$lib" ] || continue
  mkdir -p "$ROOTFS$(dirname "$lib")"
  [ -e "$ROOTFS$lib" ] || cp "$(readlink -f "$lib")" "$ROOTFS$lib"
done
for l in /lib64/ld-linux-x86-64.so.2 /lib/ld-linux-aarch64.so.1; do
  [ -f "$l" ] && [ ! -e "$ROOTFS$l" ] && { mkdir -p "$ROOTFS$(dirname "$l")"; cp "$l" "$ROOTFS$l"; }
done
cp "$(command -v busybox)" "$ROOTFS/bin/busybox"
for a in sh cat sleep true seq; do ln -s busybox "$ROOTFS/bin/$a"; done

# ---------------------------------------------------------------------------
say "1. main: use up $BURN pids, then start a daemon that keeps forking"
# ---------------------------------------------------------------------------
OUT="$("$W" init "$ROOTFS" --shell --quiet)"
SESSION="${OUT%%,*}"
echo "session: $SESSION"
wx main "for i in \$(seq $BURN); do /bin/true; done" >/dev/null
DAEMON="$(wx main '( (while :; do sleep 0.2; done) </dev/null >/dev/null 2>&1 & echo $! )')"
echo "daemon pid: $DAEMON"
assert_gt "the daemon starts above the used-up pids" "$DAEMON" "$BURN"
assert_gt "main hands out pids above the daemon before any restore" "$(new_pid main)" "$DAEMON"

# ---------------------------------------------------------------------------
say "2. checkpoint: main is restored in place"
# ---------------------------------------------------------------------------
"$W" checkpoint "$SESSION" A >/dev/null
check "the daemon survives the checkpoint" daemon_alive main
assert_gt "main hands out pids above the daemon after its restore" "$(new_pid main)" "$DAEMON"

# ---------------------------------------------------------------------------
say "3. fork: a restored tree keeps forking"
# ---------------------------------------------------------------------------
"$W" fork "$SESSION" A --id f1 >/dev/null
check "the daemon runs in f1" daemon_alive f1
assert_gt "f1 hands out pids above the daemon" "$(new_pid f1)" "$DAEMON"
assert_gt "children the daemon forks in f1 get pids above it" "$(lowest_child f1)" "$DAEMON"

# ---------------------------------------------------------------------------
say "4. snapshot f1 and fork it again"
# ---------------------------------------------------------------------------
OUT="$("$W" snapshot "$SESSION" f1 B 2>&1)" && ok "snapshot of f1, which forked after its restore" || bad "snapshot of f1 failed: $OUT"
OUT="$("$W" fork "$SESSION" B --id f2 2>&1)" && ok "fork from that snapshot" || bad "fork from B failed: $OUT"
if [ "$(wx f2 'echo up' 2>/dev/null)" = up ]; then
  check "the daemon runs in f2" daemon_alive f2
  assert_gt "f2 hands out pids above the daemon" "$(new_pid f2)" "$DAEMON"
  assert_gt "children the daemon forks in f2 get pids above it" "$(lowest_child f2)" "$DAEMON"
else
  bad "f2 does not answer exec"
fi

"$W" cleanup "$SESSION" >/dev/null
SESSION=""

COMPLETED=1
say "Summary"
echo "$PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
