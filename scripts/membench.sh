#!/bin/bash
# Measures how memory size, the share of pages dirtied between dumps and the
# write pattern affect checkpoint, snapshot and fork, with incremental dumps
# off and on. A holder process in the session keeps MEM_MB of memory; before
# every dump it rewrites PCT% of its pages, chosen at random or as one
# contiguous block. One session per pattern x incremental_dumps combination:
# a chain of DEPTH checkpoints of main, then a fork of c1 and a chain of
# DEPTH snapshots of it, with FORKS timed forks from the chain tip at every
# level, and one park. Each row reports the CLI wall clock, CRIU's own dump
# and restore times, the pages the dump wrote and skipped (from phase_stats),
# the image directory's size and the number of image sets a restore reads.
#
# Requires root, criu, a static busybox, /usr/bin/python3 (copied into the
# rootfs with its standard library for the holder) and Go unless WAYPOINT_BIN
# and BASH_INIT_BIN are given. incremental_dumps=1 needs a CRIU with
# `restore --next-images-dir`; with the stock one the first fork fails.
# CPU frequency scaling shows in the numbers, so the header names the governor.
#
#   MEM_MB=512                 MiB the holder keeps resident
#   PCT=5                      percent of its pages rewritten before each dump
#   PATTERNS="none random seq" nothing / random pages / one contiguous block
#                              that moves forward on each write
#   INCREMENTAL="0 1"          incremental_dumps values to run
#   DEPTH=5                    checkpoints per chain (and snapshots per chain)
#   FORKS=3                    timed forks from the chain tip at every level
#   NETWORK=shared             network mode; private needs pasta
#   CSV=<file>                 also append one row per level to <file>
#   WPBENCH_DIR=/tmp           where the temp dir with rootfs, sessions and
#                              images is made; a full dump writes MEM_MB there
#   WAYPOINT_BIN=<file>        binaries to time instead of building this checkout;
#   BASH_INIT_BIN=<file>       set both or neither
#
# Usage:  sudo ./scripts/membench.sh
#         sudo env MEM_MB=1024 PCT=2 PATTERNS=random ./scripts/membench.sh
set -euo pipefail
export LC_ALL=C

REPO="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d "${WPBENCH_DIR:-/tmp}/wpmem.XXXXXX")"
ROOTFS="$WORK/rootfs"
BIN="$WORK/bin"
MEM_MB="${MEM_MB:-512}"
PCT="${PCT:-5}"
PATTERNS="${PATTERNS:-none random seq}"
INCREMENTAL="${INCREMENTAL:-0 1}"
DEPTH="${DEPTH:-5}"
FORKS="${FORKS:-3}"
NETWORK="${NETWORK:-shared}"
CSV="${CSV:-}"
SESSION=""

export WAYPOINT_SESSIONS_DIR="$WORK/s" WAYPOINT_SESSION_INFO_DIR="$WORK/i"
export WAYPOINT_PRESERVE_SESSION_ON_CLEANUP=false WAYPOINT_PHASE_STATS=true
export WAYPOINT_NETWORK="$NETWORK"
# Incremental dumps are off while images live on tmpfs; keep that explicit.
export WAYPOINT_TMPFS_IMAGES="${WAYPOINT_TMPFS_IMAGES:-false}"

cleanup() {
  if [ -n "$SESSION" ]; then "$W" cleanup "$SESSION" --force >/dev/null 2>&1 || true; fi
  rm -rf "$WORK"
}
trap cleanup EXIT

for p in $PATTERNS; do
  case "$p" in none | random | seq) ;; *) echo "unknown pattern: $p (none, random or seq)"; exit 1 ;; esac
done

# run <cmd...>: runs cmd once, leaves its output in LAST, exits on failure.
run() {
  LAST="$("$@" 2>&1)" || { echo "failed: $*" >&2; echo "$LAST" >&2; exit 1; }
}
# timed <var> <cmd...>: like run, and stores the wall-clock ms in <var>.
timed() {
  local var="$1" t0; shift
  t0=$EPOCHREALTIME
  run "$@"
  printf -v "$var" '%s' "$(awk -v a="$t0" -v b="$EPOCHREALTIME" 'BEGIN { printf "%.1f", (b - a) * 1000 }')"
}
# phase <key> [default]: a value from the `phases ...` line in LAST.
phase() {
  local v
  v="$(grep -o " $1=[0-9.]*" <<<"$LAST" | head -1 | cut -d= -f2)"
  echo "${v:-${2:-0}}"
}
median() { sort -n | awk '{ v[NR] = $1 } END { if (NR) print NR % 2 ? v[(NR + 1) / 2] : (v[NR / 2] + v[NR / 2 + 1]) / 2 }'; }
# sets <ckpt>: image directories a restore from <ckpt> reads (its parent chain).
sets() {
  local d="$C/$1/criu" n=0
  while [ -d "$d" ]; do
    n=$((n + 1))
    [ -L "$d/parent" ] || break
    d="$(readlink -f "$d/parent")"
  done
  echo "$n"
}
du_mib() { du -sm "$C/$1/criu" | cut -f1; }
# write <fork>: the holder in <fork> rewrites PCT% of its pages per $P.
write() {
  run "$W" exec "$SESSION" "$1" -- "echo $P $PCT >/tmp/mem.cmd; while [ -e /tmp/mem.cmd ]; do sleep 0.05; done"
}
# forkms <ckpt>: median wall ms of FORKS forks from <ckpt>, each destroyed again.
forkms() {
  local i t
  for ((i = 1; i <= FORKS; i++)); do
    timed t "$W" fork "$SESSION" "$1" --id "tf$i"
    echo "$t"
    run "$W" destroy "$SESSION" "tf$i"
  done | median
}
# row <chain> <level> <ckpt> <wall_ms> <fork_ms>: prints one line, appends to CSV.
row() {
  local chain="$1" level="$2" ckpt="$3" wall="$4" fork="$5" n dump restore written skipped du
  n="$(sets "$ckpt")"; du="$(du_mib "$ckpt")"
  dump="$(phase dump_ms)"; restore="$(phase restore_ms -)"
  [ "$chain" != park ] || restore=-
  written="$(phase pages_written_mib)"; skipped="$(phase pages_skipped_parent_mib)"
  printf '%-5s %5s %4s %8.1f %8.1f %10s %11.1f %11.1f %7d %8s\n' \
    "$chain" "$level" "$n" "$wall" "$dump" "$restore" "$written" "$skipped" "$du" "$fork"
  [ -z "$CSV" ] || printf '%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s\n' \
    "$CSV_PREFIX" "$P" "$INC" "$chain" "$level" "$n" "$wall" "$dump" "$restore" "$written" "$skipped" "$du" "$fork" >>"$CSV"
}

# ---------------------------------------------------------------------------
# Environment, build, rootfs
# ---------------------------------------------------------------------------
[ "$(id -u)" -eq 0 ] || { echo "must run as root"; exit 1; }
for c in criu busybox; do
  command -v "$c" >/dev/null || { echo "missing required command: $c"; exit 1; }
done
if ldd "$(command -v busybox)" >/dev/null 2>&1; then
  echo "busybox must be statically linked (package busybox-static)"; exit 1
fi
[ -x /usr/bin/python3 ] || { echo "missing /usr/bin/python3 (the holder runs on it)"; exit 1; }

if [ -n "${WAYPOINT_BIN:-}" ] && [ -n "${BASH_INIT_BIN:-}" ]; then
  W="$WAYPOINT_BIN"; export WAYPOINT_BASH_INIT_SRC="$BASH_INIT_BIN"
elif [ -n "${WAYPOINT_BIN:-}${BASH_INIT_BIN:-}" ]; then
  echo "set both WAYPOINT_BIN and BASH_INIT_BIN, or neither (then this checkout is built)"; exit 1
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

# copy_libs <binary...>: the shared libraries each binary needs, at their paths.
copy_libs() {
  local b lib
  for b in "$@"; do
    { ldd "$b" 2>/dev/null | grep -oE '/[^ ]+\.so[^ ]*' || true; } | while read -r lib; do
      [ -f "$lib" ] || continue
      mkdir -p "$ROOTFS$(dirname "$lib")"
      [ -e "$ROOTFS$lib" ] || cp "$(readlink -f "$lib")" "$ROOTFS$lib"
    done
  done
}
mkdir -p "$ROOTFS"/{bin,tmp,proc,sys,root,etc,usr/bin}
cp "$(command -v bash)" "$ROOTFS/bin/bash"
cp "$(command -v busybox)" "$ROOTFS/bin/busybox"
for a in sh cat sleep true; do ln -s busybox "$ROOTFS/bin/$a"; done
PY="$(readlink -f /usr/bin/python3)"
PYLIB="$("$PY" -I -c 'import sysconfig; print(sysconfig.get_paths()["stdlib"])')"
mkdir -p "$ROOTFS$(dirname "$PY")" "$ROOTFS$(dirname "$PYLIB")"
cp "$PY" "$ROOTFS$PY"
ln -s "$(basename "$PY")" "$ROOTFS/usr/bin/python3"
cp -a "$PYLIB" "$ROOTFS$PYLIB"
copy_libs "$ROOTFS/bin/bash" "$ROOTFS$PY" "$ROOTFS$PYLIB"/lib-dynload/*.so
for l in /lib64/ld-linux-x86-64.so.2 /lib/ld-linux-aarch64.so.1; do
  [ -f "$l" ] && [ ! -e "$ROOTFS$l" ] && { mkdir -p "$ROOTFS$(dirname "$l")"; cp "$l" "$ROOTFS$l"; }
done
chroot "$ROOTFS" /usr/bin/python3 -I -c 'import random, time' || { echo "python3 does not run inside the rootfs"; exit 1; }

# The holder: MEM_MB of anonymous memory, every page touched, rewritten on
# request. A request is one line in /tmp/mem.cmd, removed when done:
#   random <pct>  rewrite pct% of the pages, chosen at random (4 KiB pages)
#   seq <pct>     rewrite pct% of the pages as one contiguous block; the
#                 block moves forward on each request and wraps around
#   none          touch nothing (still counts as a request)
#   stat          write "requests=<n>" to /tmp/mem.stat
cat >"$ROOTFS/bin/mem.py" <<'PY'
import os, random, sys, time

PAGE = 4096
npages = (int(sys.argv[1]) << 20) // PAGE
buf = bytearray(npages * PAGE)
for p in range(npages):
    buf[p * PAGE] = 1
open("/tmp/mem.pid", "w").write(str(os.getpid()))

cursor, requests = 0, 0
while True:
    try:
        cmd = open("/tmp/mem.cmd").read().split()
    except FileNotFoundError:
        time.sleep(0.02)
        continue
    if cmd[0] == "stat":
        open("/tmp/mem.stat", "w").write("requests=%d\n" % requests)
    else:
        k = int(npages * float(cmd[1]) / 100) if len(cmd) > 1 else 0
        if cmd[0] == "random":
            pages = random.sample(range(npages), k)
        elif cmd[0] == "seq":
            pages = [(cursor + i) % npages for i in range(k)]
            cursor = (cursor + k) % npages
        else:
            pages = []
        requests += 1
        for p in pages:
            buf[p * PAGE] = requests & 0xFF
    os.remove("/tmp/mem.cmd")
PY

CPU="$(awk -F': ' '/^model name/ { print $2; exit }' /proc/cpuinfo)"
[ -n "$CPU" ] || CPU="$(lscpu 2>/dev/null | awk -F': +' '/^Model name/ { print $2; exit }')"
GOVERNOR="$(cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_governor 2>/dev/null || echo unknown)"
CRIU_VERSION="$(criu --version | awk '/^Version/ { print $2; exit }')"
WP_VERSION="$("$W" version | awk '{ print $NF }')"
CSV_PREFIX="$(date -u +%FT%TZ),$(hostname -s),$(uname -r),$(echo "${CPU:-unknown}" | tr ',' ' '),$(nproc),$GOVERNOR,$CRIU_VERSION,$WP_VERSION,$MEM_MB,$PCT,$NETWORK"
if [ -n "$CSV" ] && [ ! -s "$CSV" ]; then
  echo "utc,host,kernel,cpu,nproc,governor,criu,waypoint,mem_mb,pct,network,pattern,incremental,chain,level,sets,wall_ms,dump_ms,restore_ms,written_mib,skipped_mib,du_mib,fork_ms" >"$CSV"
fi

echo
printf 'waypoint %s (%s) on %s: %s, %s x %s (%s), criu %s, %s on %s\n' \
  "$WP_VERSION" "$W" "$(hostname -s)" "$(uname -r)" "$(nproc)" "${CPU:-unknown}" "$GOVERNOR" \
  "$CRIU_VERSION" "$WORK" "$(stat -f -c %T "$WORK")"
echo "MEM_MB=$MEM_MB PCT=$PCT PATTERNS=\"$PATTERNS\" INCREMENTAL=\"$INCREMENTAL\" DEPTH=$DEPTH FORKS=$FORKS network=$NETWORK"

# ---------------------------------------------------------------------------
# One session per pattern x incremental_dumps
# ---------------------------------------------------------------------------
for P in $PATTERNS; do
  for INC in $INCREMENTAL; do
    export WAYPOINT_INCREMENTAL_DUMPS="$INC"
    run "$W" init "$ROOTFS" --shell --quiet
    SESSION="${LAST%%,*}"
    C="$WAYPOINT_SESSIONS_DIR/$SESSION/checkpoints"
    run "$W" exec "$SESSION" main -- "(python3 -I /bin/mem.py $MEM_MB </dev/null >/dev/null 2>&1 &)"
    run "$W" exec "$SESSION" main -- 'for ((i = 0; i < 1200; i++)); do [ -e /tmp/mem.pid ] && break; sleep 0.5; done; [ -e /tmp/mem.pid ]'

    echo
    echo "pattern=$P pct=$PCT incremental_dumps=$INC"
    printf '%-5s %5s %4s %8s %8s %10s %11s %11s %7s %8s\n' \
      chain level sets wall_ms dump_ms restore_ms written_MiB skipped_MiB du_MiB fork_ms
    ck=() sn=() wr=()
    # Chain of checkpoints of main: c1 is a full dump, c2.. follow the knob.
    for ((k = 1; k <= DEPTH; k++)); do
      write main
      timed ms "$W" checkpoint "$SESSION" "c$k"
      ck+=("$ms")
      row main "$k" "c$k" "$ms" "$(forkms "c$k")"
    done
    # Chain of snapshots of a fork of c1; its restore started the fork's chain.
    run "$W" fork "$SESSION" c1 --id f
    for ((k = 1; k <= DEPTH; k++)); do
      write f
      timed ms "$W" snapshot "$SESSION" f "s$k"
      sn+=("$ms"); wr+=("$(phase pages_written_mib)")
      row fork "$k" "s$k" "$ms" "$(forkms "s$k")"
    done
    run "$W" fork "$SESSION" c1 --id p
    write p
    timed ms "$W" snapshot "$SESSION" p pk --park
    row park - pk "$ms" -
    # The holders came back with their state: main saw DEPTH requests, f saw
    # the one before c1 plus DEPTH of its own.
    for want in "main $DEPTH" "f $((DEPTH + 1))"; do
      read -r who n <<<"$want"
      run "$W" exec "$SESSION" "$who" -- 'echo stat >/tmp/mem.cmd; while [ -e /tmp/mem.cmd ]; do sleep 0.05; done; cat /tmp/mem.stat'
      [ "$LAST" = "requests=$n" ] || { echo "holder in $who reports '$LAST', want requests=$n" >&2; exit 1; }
    done
    printf 'summary: checkpoint 2..%d %.1f ms, snapshot %.1f ms, fork from c1 %s ms, from s%d %s ms, park %.1f ms; snapshot dump %s MiB, checkpoints %d MiB\n' \
      "$DEPTH" "$(printf '%s\n' "${ck[@]:1}" | median)" "$(printf '%s\n' "${sn[@]}" | median)" \
      "$(forkms c1)" "$DEPTH" "$(forkms "s$DEPTH")" "$ms" "$(printf '%s\n' "${wr[@]}" | median)" "$(du -sm "$C" | cut -f1)"

    run "$W" cleanup "$SESSION"; SESSION=""
    left="$(grep -c " $WORK/" /proc/self/mountinfo || true)"
    [ "$left" -eq 0 ] || { echo "cleanup left $left mounts under $WORK" >&2; exit 1; }
  done
done
