#!/bin/bash
# Times `waypoint diff` and `waypoint blame` on one session, for comparing a
# change with the build before it. With BASELINE_BIN, the two builds run in
# turn on the same checkpoints and the table gives both medians and their
# ratio; without it, the table gives this build's median, min and max. It
# checks only that every command succeeds.
#
# Requires root, criu, a static busybox, and Go unless WAYPOINT_BIN and
# BASH_INIT_BIN are given. Sessions, registry and rootfs live in a private temp
# dir. Every diff runs without its cached result. Most cases take a few
# milliseconds, so on a busy host single cases can be off by tens of percent:
# with this build's own binary as BASELINE_BIN the ratios show that spread.
#
#   RUNS=9                 runs per case and build
#   FILES=2000             small files in the directory that the cases touch,
#                          copy and remove
#   BIG_MB=8               size of each of the three large files
#   CKPTS=10               checkpoints that change the large files
#   BASELINE_BIN=<file>    the waypoint binary to compare with, e.g. the base
#                          commit built in another worktree:
#                            git worktree add /tmp/base <commit>
#                            (cd /tmp/base && ./setup build)
#                          gives /tmp/base/bin/waypoint
#   WPBENCH_DIR=/tmp       where the temp dir is made; it grows to about
#                          3 * BIG_MB * CKPTS MiB
#   WAYPOINT_BIN=<file>    binaries to time instead of building this checkout;
#   BASH_INIT_BIN=<file>   set both or neither
#
# Usage:  sudo ./scripts/bench-diff-blame.sh
#         sudo env BASELINE_BIN=/tmp/base/bin/waypoint ./scripts/bench-diff-blame.sh
set -euo pipefail
export LC_ALL=C

REPO="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d "${WPBENCH_DIR:-/tmp}/wpbench.XXXXXX")"
ROOTFS="$WORK/rootfs"
# shellcheck disable=SC2034  # read by lib/bench-env.sh
BIN="$WORK/bin"
RUNS="${RUNS:-9}"
FILES="${FILES:-2000}"
BIG_MB="${BIG_MB:-8}"
CKPTS="${CKPTS:-10}"
BASELINE_BIN="${BASELINE_BIN:-}"
# shellcheck disable=SC2034  # read by lib/bench-env.sh
CSV="" LABEL=""
SESSION=""
# shellcheck source=lib/bench-env.sh
. "$REPO/scripts/lib/bench-env.sh"

cleanup() {
  [ -n "$SESSION" ] && "$W" cleanup "$SESSION" --force >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

bench_require
bench_binaries
if [ -n "$BASELINE_BIN" ] && [ ! -x "$BASELINE_BIN" ]; then
  echo "BASELINE_BIN is not an executable file: $BASELINE_BIN"; exit 1
fi
export WAYPOINT_NETWORK=shared

# ---------------------------------------------------------------------------
# Rootfs: /w/small.txt, FILES small files in /w/many, three large files
# ---------------------------------------------------------------------------
mkdir -p "$ROOTFS"/{bin,tmp,proc,sys,root,etc,w/many}
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
for a in sh cat cp dd rm touch true; do ln -s busybox "$ROOTFS/bin/$a"; done

for ((i = 0; i < 20; i++)); do echo "line $i"; done >"$ROOTFS/w/small.txt"
for ((i = 0; i < FILES; i++)); do echo "file $i" >"$ROOTFS/w/many/f$i"; done
awk -v n="$((BIG_MB * 1048576 / 75))" 'BEGIN {
  for (i = 0; i < n; i++) printf "%07d 0123456789 0123456789 0123456789 0123456789 0123456789 0123456789\n", i
}' >"$ROOTFS/w/appended.txt"
cp "$ROOTFS/w/appended.txt" "$ROOTFS/w/touched.txt"
head -c "$((BIG_MB * 1048576))" /dev/urandom >"$ROOTFS/w/data.bin"

bench_env
echo "RUNS=$RUNS FILES=$FILES BIG_MB=$BIG_MB CKPTS=$CKPTS${BASELINE_BIN:+ BASELINE_BIN=$BASELINE_BIN}"

# ---------------------------------------------------------------------------
# One session: a chain that changes the large files, then four checkpoints
# that change, touch, copy and remove the small files
# ---------------------------------------------------------------------------
SESSION="$("$W" init "$ROOTFS" --shell --quiet)"; SESSION="${SESSION%%,*}"
wx() { "$W" exec "$SESSION" main -- "$1" >/dev/null; }
ck() { "$W" checkpoint "$SESSION" "$1" >/dev/null; }

ck c0
for ((i = 1; i <= CKPTS; i++)); do
  wx "echo 'appended $i' >>/w/appended.txt; touch /w/touched.txt
      dd if=/dev/zero of=/w/data.bin bs=4096 seek=$i count=1 conv=notrunc 2>/dev/null
      [ $((i % 3)) -ne 0 ] || echo 'edit $i' >>/w/small.txt"
  ck "c$i"
done
LAST="c$CKPTS"
wx 'echo a >/w/many/f1; echo b >/w/many/f2; echo c >/w/many/f3'; ck few
wx 'touch /w/many/*'; ck touched
wx 'cp -r /w/many /w/many2'; ck copied
wx 'rm -rf /w/many2'; ck removed

# "<name>|<diff or blame> <arguments after the session>"
CASES=(
  "diff: 3 files changed|diff $LAST few"
  "diff: $FILES files touched, same content|diff few touched"
  "diff: $FILES files added|diff touched copied"
  "diff: $FILES files removed|diff copied removed"
  "diff: large files changed over $CKPTS checkpoints|diff c0 $LAST"
  "blame: small file|blame $LAST /w/small.txt"
  "blame: file beside $FILES others|blame removed /w/many/f1"
  "blame: large file touched in every checkpoint|blame $LAST /w/touched.txt"
  "blame: large file, one line appended each time|blame $LAST /w/appended.txt"
  "blame: large binary file|blame $LAST /w/data.bin"
)
BUILDS=("$W")
[ -z "$BASELINE_BIN" ] || BUILDS=("$BASELINE_BIN" "$W")

# run_case <binary> <key> <diff|blame> <args...>: runs it once and appends its
# wall-clock ms to $WORK/t.<key>.
run_case() {
  local bin="$1" key="$2" t0; shift 2
  [ "$1" != diff ] || rm -rf "$WORK/s/$SESSION/diffs"
  t0=$EPOCHREALTIME
  "$bin" "$1" "$SESSION" "${@:2}" >/dev/null 2>"$WORK/err" ||
    { echo "$bin $* failed: $(cat "$WORK/err")" >&2; exit 1; }
  awk -v a="$t0" -v b="$EPOCHREALTIME" 'BEGIN { printf "%.1f\n", (b - a) * 1000 }' >>"$WORK/t.$key"
}

# The builds take turns within every run, so that drift hits both alike.
for ((c = 0; c < ${#CASES[@]}; c++)); do
  read -r -a argv <<<"${CASES[c]#*|}"
  for ((r = 0; r < RUNS; r++)); do
    for ((b = 0; b < ${#BUILDS[@]}; b++)); do run_case "${BUILDS[b]}" "$c.$b" "${argv[@]}"; done
  done
done

# stats <file>: prints "median min max".
stats() {
  sort -n "$1" | awk '{ v[NR] = $1 }
    END { printf "%.1f %.1f %.1f\n", (NR % 2 ? v[(NR + 1) / 2] : (v[NR / 2] + v[NR / 2 + 1]) / 2), v[1], v[NR] }'
}

echo
if [ -n "$BASELINE_BIN" ]; then
  printf '%-48s %11s %11s %7s   (median ms; ratio = this / baseline)\n' case baseline this ratio
else
  printf '%-48s %9s %9s %9s   (ms)\n' case median min max
fi
for ((c = 0; c < ${#CASES[@]}; c++)); do
  read -r median min max <<<"$(stats "$WORK/t.$c.$((${#BUILDS[@]} - 1))")"
  if [ -n "$BASELINE_BIN" ]; then
    read -r base _ _ <<<"$(stats "$WORK/t.$c.0")"
    printf '%-48s %11.1f %11.1f %7.2f\n' "${CASES[c]%%|*}" "$base" "$median" \
      "$(awk -v a="$median" -v b="$base" 'BEGIN { print (b > 0 ? a / b : 0) }')"
  else
    printf '%-48s %9.1f %9.1f %9.1f\n' "${CASES[c]%%|*}" "$median" "$min" "$max"
  fi
done
