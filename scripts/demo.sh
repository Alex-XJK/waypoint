#!/bin/bash
# End-to-end demo of Waypoint parallel forking with a persistent shell.
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

say()  { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
run()  { printf '\033[0;90m$ %s\033[0m\n' "$*"; eval "$*"; }

cleanup() {
  [ -n "$SESSION" ] && "$BIN/waypoint" cleanup "$SESSION" --force >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

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
mkdir -p "$ROOTFS"/{bin,tmp,proc,sys,dev,root}
for b in bash cat ls sleep mkdir rm ps grep; do
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
# dynamic loader + /dev/null
for l in /lib/ld-linux-aarch64.so.1 /lib64/ld-linux-x86-64.so.2; do
  [ -f "$l" ] && { mkdir -p "$ROOTFS$(dirname "$l")"; cp -n "$l" "$ROOTFS$l"; }
done
mknod "$ROOTFS/dev/null" c 1 3 2>/dev/null || true; chmod 666 "$ROOTFS/dev/null"
echo "rootfs: $ROOTFS ($(du -sh "$ROOTFS" | cut -f1))"

W="$BIN/waypoint"

# ---------------------------------------------------------------------------
say "1. init a session with a live shell (the 'main' fork)"
# ---------------------------------------------------------------------------
OUT="$("$W" init "$ROOTFS" --shell --quiet)"
SESSION="${OUT%%,*}"
echo "session: $SESSION"

# ---------------------------------------------------------------------------
say "2. build hidden shell state: cwd, a variable, a background job"
# ---------------------------------------------------------------------------
run "$W exec $SESSION main -- 'cd /root; GREETING=hello-from-checkpoint; sleep 600 & echo started job \$!'"

# ---------------------------------------------------------------------------
say "3. checkpoint main -> immutable checkpoint A"
# ---------------------------------------------------------------------------
run "$W checkpoint $SESSION A"

# ---------------------------------------------------------------------------
say "4. hidden state survives restore (cwd + var + running job)"
# ---------------------------------------------------------------------------
run "$W exec $SESSION main -- 'echo cwd=\$(pwd) GREETING=\$GREETING; jobs -r'"

# ---------------------------------------------------------------------------
say "5. fork A into two independent live shells"
# ---------------------------------------------------------------------------
run "$W fork $SESSION A --id f1"
run "$W fork $SESSION A --id f2"

# ---------------------------------------------------------------------------
say "6. forks inherit the state, then diverge without touching each other"
# ---------------------------------------------------------------------------
run "$W exec $SESSION f1 -- 'echo f1 sees GREETING=\$GREETING cwd=\$(pwd)'"
run "$W exec $SESSION f2 -- 'GREETING=f2-only; cd /tmp; echo f2 changed its own state'"
run "$W exec $SESSION f1 -- 'echo f1 still: GREETING=\$GREETING cwd=\$(pwd)'"
run "$W exec $SESSION f2 -- 'echo f2 now:   GREETING=\$GREETING cwd=\$(pwd)'"

# ---------------------------------------------------------------------------
say "7. exit codes propagate"
# ---------------------------------------------------------------------------
run "$W exec $SESSION f1 -- 'ls /does-not-exist' || echo \"  -> waypoint exec exited \$?\""

# ---------------------------------------------------------------------------
say "8. recursive: snapshot f2's diverged state -> B, fork B -> f3"
# ---------------------------------------------------------------------------
run "$W snapshot $SESSION f2 B"
run "$W fork $SESSION B --id f3"
run "$W exec $SESSION f3 -- 'echo f3 inherited: GREETING=\$GREETING cwd=\$(pwd)'"

# ---------------------------------------------------------------------------
say "9. concurrency: two 2s commands on different forks finish in ~2s"
# ---------------------------------------------------------------------------
start=$(date +%s)
"$W" exec "$SESSION" f1 -- 'sleep 2; echo f1 done' &
"$W" exec "$SESSION" f3 -- 'sleep 2; echo f3 done' &
wait
echo "  -> wall clock: $(( $(date +%s) - start ))s"

# ---------------------------------------------------------------------------
say "10. inspect the DAG (machine-readable)"
# ---------------------------------------------------------------------------
run "$W list $SESSION --json | grep -E '\"id\"|\"status\"|\"base_checkpoint_id\"'"

say "Demo complete — cleaning up"
