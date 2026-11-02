#!/bin/bash
# Times Waypoint's basic operations on a small busybox rootfs. Each operation
# runs RUNS times and is reported as median/min/max wall-clock ms, as seen from
# the CLI. For comparing hosts and spotting regressions; it checks only that
# every operation succeeds and that cleanup leaves no mounts or pasta behind.
#
# Requires root, criu, a static busybox, and Go unless WAYPOINT_BIN and
# BASH_INIT_BIN are given; pasta for the private network mode; python3 to
# read the effective config from `waypoint info`. Sessions, registry and
# rootfs live in a private temp dir. lib/bench-env.sh pins tmpfs_images,
# incremental_dumps and phase_stats off and the log level to 2 unless the
# environment sets them (WAYPOINT_TMPFS_IMAGES etc.); the host's config file
# does not apply to them. CPU frequency scaling shows in the numbers, so the
# header names the cpufreq governor.
#
#   RUNS=10                   runs per operation
#   FORKS=8                   forks per `fork --n`
#   NETWORK="shared private"  network modes to time; private needs pasta
#   MEM_MB=0                  MiB a background process in main holds, so that
#                             dumps and restores copy that much memory
#   WPBENCH_DIR=/tmp          where the temp dir with rootfs, sessions and
#                             images is made; a full dump writes MEM_MB there
#   CSV=<file>                also append one row per operation to <file>
#   LABEL=<text>              free text for the CSV's label column, e.g. the
#                             change under test; commas become spaces
#   WAYPOINT_BIN=<file>       binaries to time instead of building this checkout;
#   BASH_INIT_BIN=<file>      set both or neither
#
# Every CSV row starts with the environment columns of lib/bench-env.sh
# (utc, label, host, kernel, cpu, nproc, governor, images_fs, criu,
# criu_gitid, waypoint, waypoint_commit, log_level), then the effective
# incremental_dumps value. A CSV written by an older version of the script
# has a different header and is refused.
#
# Usage:  sudo ./scripts/benchmark.sh
#         sudo env LABEL=my-change CSV=runs.csv ./scripts/benchmark.sh
#         sudo env WAYPOINT_BIN=... BASH_INIT_BIN=... ./scripts/benchmark.sh
set -euo pipefail
export LC_ALL=C

REPO="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d "${WPBENCH_DIR:-/tmp}/wpbench.XXXXXX")"
ROOTFS="$WORK/rootfs"
# shellcheck disable=SC2034  # read by lib/bench-env.sh
BIN="$WORK/bin"
RUNS="${RUNS:-10}"
FORKS="${FORKS:-8}"
NETWORK="${NETWORK:-shared private}"
MEM_MB="${MEM_MB:-0}"
CSV="${CSV:-}"
LABEL="${LABEL:-}"
SESSION=""
SESSIONS=()
# shellcheck source=lib/bench-env.sh
. "$REPO/scripts/lib/bench-env.sh"
HEADER="$ENV_HEADER,incremental,mem_mb,network,operation,runs,median_ms,min_ms,max_ms"

cleanup() {
  [ -n "$SESSION" ] && "$W" cleanup "$SESSION" --force >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

# timed <op> <cmd...>: runs cmd once, appends its wall-clock ms to $WORK/t.<op>,
# and leaves its output in LAST.
timed() {
  local op="$1" t0; shift
  t0=$EPOCHREALTIME
  LAST="$("$@" 2>&1)" || { echo "$op failed: $LAST" >&2; exit 1; }
  awk -v a="$t0" -v b="$EPOCHREALTIME" 'BEGIN { printf "%.1f\n", (b - a) * 1000 }' >>"$WORK/t.$op"
}

OPS=(init "init --shell" exec "exec true" cp_in "cp 1 MiB in" cp_out "cp 1 MiB out"
  checkpoint "checkpoint" fork "fork" fork_n "fork --n $FORKS" snapshot "snapshot"
  park "snapshot --park" destroy "destroy" cleanup "cleanup")

# report <network>: prints, and optionally appends to CSV, one line per operation.
report() {
  local i key
  printf '\n%-18s %5s %9s %9s %9s   (ms, network=%s)\n' operation runs median min max "$1"
  for ((i = 0; i < ${#OPS[@]}; i += 2)); do
    key="${OPS[i]}"
    sort -n "$WORK/t.$key" | awk -v op="${OPS[i + 1]}" -v csv="$CSV" -v row="$CSV_PREFIX,$1" '
      { v[NR] = $1 }
      END {
        med = NR % 2 ? v[(NR + 1) / 2] : (v[NR / 2] + v[NR / 2 + 1]) / 2
        printf "%-18s %5d %9.1f %9.1f %9.1f\n", op, NR, med, v[1], v[NR]
        if (csv != "") printf "%s,%s,%d,%.1f,%.1f,%.1f\n", row, op, NR, med, v[1], v[NR] >>csv
      }'
    rm -f "$WORK/t.$key"
  done
}

# ---------------------------------------------------------------------------
# Environment, build, rootfs
# ---------------------------------------------------------------------------
bench_require
bench_csv_header "$HEADER"
bench_binaries

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
for a in sh cat head tr sleep true; do ln -s busybox "$ROOTFS/bin/$a"; done
# hold <bytes>: keeps that many bytes resident. The sleep loops because a
# busybox sleep cut short by a dump exits instead of resuming.
cat >"$ROOTFS/bin/hold" <<'EOF'
#!/bin/sh
echo $$ >/tmp/hold.pid
x=$(head -c "$1" /dev/zero | tr '\000' a)
: >/tmp/hold.ready
while :; do sleep 3600; done
EOF
chmod +x "$ROOTFS/bin/hold"
head -c 1048576 /dev/urandom >"$WORK/1m"

bench_env
INCREMENTAL="$(wpinfo incremental_dumps)"
echo "RUNS=$RUNS FORKS=$FORKS MEM_MB=$MEM_MB incremental_dumps=$INCREMENTAL${LABEL:+ LABEL=$LABEL}"
CSV_PREFIX="$ENV_PREFIX,$INCREMENTAL,$MEM_MB"

# ---------------------------------------------------------------------------
# Operations, per network mode
# ---------------------------------------------------------------------------
for mode in $NETWORK; do
  if [ "$mode" = private ] && ! command -v pasta >/dev/null; then
    echo; echo "network=private skipped: pasta not found (package passt)"; continue
  fi
  export WAYPOINT_NETWORK="$mode"

  # The last session stays for the operations below.
  for ((i = 1; i <= RUNS; i++)); do
    timed init "$W" init "$ROOTFS" --shell --quiet
    SESSION="${LAST%%,*}"; SESSIONS+=("$SESSION")
    if [ "$i" -lt "$RUNS" ]; then timed cleanup "$W" cleanup "$SESSION"; SESSION=""; fi
  done
  if [ "$MEM_MB" -gt 0 ]; then
    "$W" exec "$SESSION" main -- "(hold $((MEM_MB * 1048576)) </dev/null >/dev/null 2>&1 &)" >/dev/null
    "$W" exec "$SESSION" main -- 'for ((i = 0; i < 600; i++)); do [ -e /tmp/hold.ready ] && break; sleep 1; done; [ -e /tmp/hold.ready ]' >/dev/null
  fi

  for ((i = 1; i <= RUNS; i++)); do timed exec "$W" exec "$SESSION" main -- true; done
  for ((i = 1; i <= RUNS; i++)); do
    timed cp_in "$W" cp "$SESSION" "$WORK/1m" "main:/tmp/1m.$i"
    timed cp_out "$W" cp "$SESSION" "main:/tmp/1m.$i" "$WORK/out.$i"
  done
  for ((i = 1; i <= RUNS; i++)); do timed checkpoint "$W" checkpoint "$SESSION" "c$i"; done
  for ((i = 1; i <= RUNS; i++)); do timed fork "$W" fork "$SESSION" c1 --id "f$i"; done
  for ((i = 1; i <= RUNS; i++)); do timed snapshot "$W" snapshot "$SESSION" "f$i" "s$i"; done
  for ((i = 1; i <= RUNS; i++)); do timed destroy "$W" destroy "$SESSION" "f$i"; done
  for ((i = 1; i <= RUNS; i++)); do
    timed fork_n "$W" fork "$SESSION" c1 --n "$FORKS"
    for f in $(awk '{ print $1 }' <<<"$LAST"); do "$W" destroy "$SESSION" "$f" >/dev/null; done
  done
  for ((i = 1; i <= RUNS; i++)); do
    "$W" fork "$SESSION" c1 --id "p$i" >/dev/null
    timed park "$W" snapshot "$SESSION" "p$i" "pk$i" --park
  done
  if [ "$MEM_MB" -gt 0 ] && ! "$W" exec "$SESSION" main -- 'kill -0 "$(cat /tmp/hold.pid)"' >/dev/null; then
    echo "the memory holder in main did not survive" >&2; exit 1
  fi
  timed cleanup "$W" cleanup "$SESSION"; SESSION=""
  rm -f "$WORK"/out.*

  report "$mode"

  left="$(grep -c " $WORK/" /proc/self/mountinfo || true)"
  [ "$left" -eq 0 ] || { echo "cleanup left $left mounts under $WORK" >&2; exit 1; }
  for s in "${SESSIONS[@]}"; do
    if [ -e "/run/waypoint/$s" ] || pgrep -f -- "--netns /run/waypoint/$s/" >/dev/null; then
      echo "cleanup left network state for session $s" >&2; exit 1
    fi
  done
done
