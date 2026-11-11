#!/bin/bash
# Runtime test of `waypoint diff` between checkpoints of a busybox rootfs.
#
# Takes checkpoint A, changes files under /data in main, takes checkpoint B, and
# checks the JSON from `diff`: its shape, the records for each change, changes
# that leave the final content the same, argument errors, and refusal to diff a
# checkpoint that is not ready. TODO lines print changes `diff` does not report
# yet; they do not count as failures.
#
# Requires root, criu, a static busybox, python3, and Go unless WAYPOINT_BIN and
# BASH_INIT_BIN are given. Sessions, registry and rootfs live in a private temp
# dir.
#
# Usage:  sudo ./scripts/test-diff.sh
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d /tmp/wpdiff.XXXXXX)"
ROOTFS="$WORK/rootfs"
BIN="$WORK/bin"
OUT="$WORK/out"
SESSION=""
CURRENT_SECTION="startup"
COMPLETED=0
PASS=0; FAIL=0; TODO=0

export WAYPOINT_SESSIONS_DIR="$WORK/s" WAYPOINT_SESSION_INFO_DIR="$WORK/i"
export WAYPOINT_NETWORK=shared
export WAYPOINT_PRESERVE_SESSION_ON_CLEANUP=false

say()  { CURRENT_SECTION="$*"; printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
ok()   { PASS=$((PASS+1)); printf '   \033[0;32mok\033[0m  %s\n' "$*"; }
bad()  { FAIL=$((FAIL+1)); printf '   \033[0;31mFAIL\033[0m %s\n' "$*"; }
todo() { TODO=$((TODO+1)); printf '   \033[0;33mTODO\033[0m %s\n' "$*"; }
check() { local desc="$1"; shift; if "$@"; then ok "$desc"; else bad "$desc"; fi; }
later() { local desc="$1"; shift; if "$@"; then ok "$desc"; else todo "$desc"; fi; }

# Unmounts anything a failed diff left under $WORK before removing it.
cleanup() {
  if [ "$COMPLETED" -ne 1 ]; then
    printf '\n\033[0;31m!! aborted during "%s"\033[0m\n' "$CURRENT_SECTION"
  fi
  [ -n "$SESSION" ] && "$W" cleanup "$SESSION" --force >/dev/null 2>&1 || true
  awk -v w="$WORK" 'index($5, w "/") == 1 {print $5}' /proc/self/mountinfo |
    sort -r | while read -r mp; do umount -l "$mp" 2>/dev/null || true; done
  rm -rf --one-file-system "$WORK"
}
trap cleanup EXIT

wx() { "$W" exec "$SESSION" main -- "$1"; }
# diff_to <name> <args...>: runs diff, saves stdout to $OUT/<name>.json, and
# returns diff's exit status.
diff_to() { local name="$1"; shift; "$W" diff "$SESSION" "$@" >"$OUT/$name.json" 2>"$OUT/$name.err"; }
# field <name> <python expression on j>: prints a value from a saved result.
field() { python3 -c 'import json,sys; j=json.load(open(sys.argv[1])); print(eval(sys.argv[2]))' "$OUT/$1.json" "$2" 2>/dev/null; }
# record <name> <path>: prints "change_type file_type" for path, or nothing.
record() {
  python3 - "$OUT/$1.json" "$2" <<'EOF' 2>/dev/null
import json, sys
for r in json.load(open(sys.argv[1]))["filesystem"]["records"]:
    if r["path"] == sys.argv[2]:
        print(r["change_type"], r["file_type"])
EOF
}
has_record() { [ "$(record "$1" "$2")" = "$3" ]; }
no_record() { [ -z "$(record "$1" "$2")" ]; }
any_record() { [ -n "$(record "$1" "$2")" ]; }
records_contain() { grep -q "$2" "$OUT/$1.json"; }
not_only_added() { local r; r="$(record "$1" "$2")"; [ -n "$r" ] && [ "$r" != "added regular" ]; }
fails() { ! "$@"; }

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
mkdir -p "$ROOTFS"/{bin,tmp,proc,sys,root,etc} "$OUT"
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
for a in sh cat true chmod ln mkdir rm rmdir touch; do ln -s busybox "$ROOTFS/bin/$a"; done

D="$ROOTFS/data"
mkdir -p "$D"/{sub,dirperm,todir}
echo same >"$D/same.txt"; echo v1 >"$D/mod.txt"; echo d >"$D/del.txt"; echo c >"$D/chmod.txt"
echo s >"$D/sub/keep"; ln -s same.txt "$D/link"
ln -s /data "$ROOTFS/abslink"

# ---------------------------------------------------------------------------
say "1. checkpoint A, change /data in main, checkpoint B"
# ---------------------------------------------------------------------------
OUTPUT="$("$W" init "$ROOTFS" --shell --quiet)"
SESSION="${OUTPUT%%,*}"
echo "session: $SESSION"
"$W" checkpoint "$SESSION" A >/dev/null
wx 'cd /data && echo v2 >mod.txt && rm del.txt && echo n >new.txt && chmod 600 chmod.txt &&
    ln -sf mod.txt link && echo same >same.txt && echo t >gone.txt && rm gone.txt &&
    touch sub/keep && echo s2 >sub/new2 && chmod 700 dirperm && mkdir emptydir &&
    rmdir todir && echo x >todir' >/dev/null
"$W" checkpoint "$SESSION" B >/dev/null

# ---------------------------------------------------------------------------
say "2. diff A B --dir /data"
# ---------------------------------------------------------------------------
check "diff exits 0" diff_to ab A B --dir /data
check "from is A" [ "$(field ab 'j["from"]')" = A ]
check "to is B" [ "$(field ab 'j["to"]')" = B ]
check "filesystem.target_dir is /data" [ "$(field ab 'j["filesystem"]["target_dir"]')" = /data ]
check "content change: /data/mod.txt modified" has_record ab /data/mod.txt "modified regular"
check "delete: /data/del.txt deleted" has_record ab /data/del.txt "deleted regular"
check "new file: /data/new.txt added" has_record ab /data/new.txt "added regular"
check "chmod: /data/chmod.txt modified" has_record ab /data/chmod.txt "modified regular"
check "symlink retarget: /data/link modified" has_record ab /data/link "modified symlink"
check "new file in a subdirectory: /data/sub/new2 added" has_record ab /data/sub/new2 "added regular"
check "same-content rewrite: no record for /data/same.txt" no_record ab /data/same.txt
check "created then deleted: no record for /data/gone.txt" no_record ab /data/gone.txt
check "touch only: no record for /data/sub/keep" no_record ab /data/sub/keep
later "new empty directory: /data/emptydir reported" any_record ab /data/emptydir
later "chmod on a directory: /data/dirperm reported" any_record ab /data/dirperm
later "empty directory replaced by a file: /data/todir reported as more than added" \
  not_only_added ab /data/todir

# ---------------------------------------------------------------------------
say "3. diff B A and diff A A"
# ---------------------------------------------------------------------------
check "diff B A exits 0" diff_to ba B A --dir /data
check "reverse: /data/new.txt deleted" has_record ba /data/new.txt "deleted regular"
check "reverse: /data/del.txt added" has_record ba /data/del.txt "added regular"
check "diff A A exits 0" diff_to aa A A
check "diff A A has no records" [ "$(field aa 'len(j["filesystem"]["records"])')" = 0 ]

# ---------------------------------------------------------------------------
say "4. arguments and checkpoint status"
# ---------------------------------------------------------------------------
# /abslink -> /data inside the rootfs; the host has no /data/sub/new2.
if diff_to abslink A B --dir /abslink/sub; then
  check "--dir through an absolute symlink: error or the container's /data/sub/new2" \
    records_contain abslink new2
else
  ok "--dir through an absolute symlink: error ($(head -n 1 "$OUT/abslink.err"))"
fi
check "unknown flag is rejected" fails diff_to badflag A B --dri /data
check "--dir without a value is rejected" fails diff_to novalue A B --dir
"$W" checkpoint "$SESSION" C >/dev/null
python3 - "$WORK/s/$SESSION/metadata/C.json" <<'EOF'
import json, sys
path = sys.argv[1]
meta = json.load(open(path))
meta["status"] = "failed"
json.dump(meta, open(path, "w"), indent=2)
EOF
check "a failed checkpoint is refused" fails diff_to failed A C
check "diff C C on a failed checkpoint is refused" fails diff_to failedsame C C
check "diff NOPE NOPE is refused" fails diff_to nope NOPE NOPE

"$W" cleanup "$SESSION" >/dev/null
SESSION=""

COMPLETED=1
say "Summary"
echo "$PASS passed, $FAIL failed, $TODO todo"
[ "$FAIL" -eq 0 ]
