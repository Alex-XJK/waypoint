#!/bin/bash
# Runtime test of `waypoint diff` between checkpoints of a busybox rootfs.
#
# Takes checkpoint A, changes files under /data in main, takes checkpoint B, and
# checks the JSON from `diff`: its shape, the records for each change, changes
# that leave the final content the same, argument errors, and refusal to diff a
# checkpoint that is not ready. Section 5 checks that `processes` reports the
# pids of jobs killed and started between two checkpoints. Section 6 checks the
# `shell` records for cd, umask and stty. Section 7 checks the result cache.
# TODO lines print changes `diff` does not report yet; they do not
# count as failures.
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
# proc <name> <pid>: prints the change_type of the process record for pid.
proc() {
  python3 - "$OUT/$1.json" "$2" <<'EOF' 2>/dev/null
import json, sys
for r in json.load(open(sys.argv[1]))["processes"]:
    if str(r["pid"]) == sys.argv[2]:
        print(r["change_type"])
EOF
}
has_proc() { [ -n "$2" ] && [ "$(proc "$1" "$2")" = "$3" ]; }
any_proc() { [ -n "$2" ] && [ -n "$(proc "$1" "$2")" ]; }
# shell_change <name> <kind> [<tty name>]: prints the from and to value of
# that shell record as JSON, or nothing.
shell_change() {
  python3 - "$OUT/$1.json" "$2" "${3:-}" <<'EOF' 2>/dev/null
import json, sys
for r in json.load(open(sys.argv[1]))["shell"]["records"]:
    if r["kind"] == sys.argv[2] and r.get("name", "") == sys.argv[3]:
        print(json.dumps(r["from"]), json.dumps(r["to"]))
EOF
}
not_added_or_deleted() { local c; c="$(proc "$1" "$2")"; [ -n "$2" ] && [ "$c" != added ] && [ "$c" != deleted ]; }
# start_job <tag>: starts a sh loop in main's background and prints its pid.
# The loops differ only by tag; busybox sleep itself exits after a checkpoint.
start_job() {
  wx "sh -c 'while :; do sleep 1000; done # $1' >/dev/null 2>&1 & echo \"pid=\$!\"" |
    sed -n 's/^pid=//p'
}
fails() { ! "$@"; }
# no_mount_under <dir>: true when nothing is mounted below dir.
no_mount_under() { ! awk -v w="$1" 'index($5, w "/") == 1 {found = 1} END {exit !found}' /proc/self/mountinfo; }

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
for a in sh cat true chmod ln mkdir rm rmdir stty touch; do ln -s busybox "$ROOTFS/bin/$a"; done

D="$ROOTFS/data"
mkdir -p "$D"/{sub,dirperm,todir}
echo same >"$D/same.txt"; echo v1 >"$D/mod.txt"; echo d >"$D/del.txt"; echo c >"$D/chmod.txt"
echo b >"$D/both.txt"
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
    echo b2 >both.txt && chmod 600 both.txt &&
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
check "chmod only: /data/chmod.txt metadata" has_record ab /data/chmod.txt "metadata regular"
check "chmod and content change: /data/both.txt modified" has_record ab /data/both.txt "modified regular"
check "symlink retarget: /data/link modified" has_record ab /data/link "modified symlink"
check "new file in a subdirectory: /data/sub/new2 added" has_record ab /data/sub/new2 "added regular"
check "same-content rewrite: no record for /data/same.txt" no_record ab /data/same.txt
check "created then deleted: no record for /data/gone.txt" no_record ab /data/gone.txt
check "touch only: no record for /data/sub/keep" no_record ab /data/sub/keep
check "new empty directory: /data/emptydir added" has_record ab /data/emptydir "added directory"
check "chmod on a directory: /data/dirperm metadata" has_record ab /data/dirperm "metadata directory"
check "empty directory replaced by a file: /data/todir is a type change" \
  has_record ab /data/todir "modified modified"
check "the target directory itself is not reported" no_record ab /data

# ---------------------------------------------------------------------------
say "3. diff B A and diff A A"
# ---------------------------------------------------------------------------
check "diff B A exits 0" diff_to ba B A --dir /data
check "reverse: /data/new.txt deleted" has_record ba /data/new.txt "deleted regular"
check "reverse: /data/del.txt added" has_record ba /data/del.txt "added regular"
check "reverse: /data/chmod.txt metadata" has_record ba /data/chmod.txt "metadata regular"
check "reverse: /data/emptydir deleted" has_record ba /data/emptydir "deleted directory"
check "diff A A exits 0" diff_to aa A A
check "diff A A has no records" [ "$(field aa 'len(j["filesystem"]["records"])')" = 0 ]
check "diff A A has no process records" [ "$(field aa 'len(j["processes"])')" = 0 ]

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

# ---------------------------------------------------------------------------
say "5. processes: checkpoint P, kill one job and start another, checkpoint Q"
# ---------------------------------------------------------------------------
# Pids are the identity for now. A CRIU without the ns_last_pid fix reuses
# pids after a checkpoint, so a reused pid shows up as modified: the checks
# only need each pid to be reported, and the expected change type is a TODO.
KEEP="$(start_job keep)"; GONE="$(start_job gone)"
"$W" checkpoint "$SESSION" P >/dev/null
NEW="$(start_job new)"
wx "kill $GONE; wait $GONE" >/dev/null 2>&1 || true
echo "jobs: keep=$KEEP gone=$GONE new=$NEW"
"$W" checkpoint "$SESSION" Q >/dev/null
check "diff P Q exits 0" diff_to pq P Q
check "processes is a list" [ "$(field pq 'type(j["processes"]).__name__')" = list ]
check "the root directory is not reported" no_record pq /
check "killed job: pid $GONE reported" any_proc pq "$GONE"
check "new job: pid $NEW reported" any_proc pq "$NEW"
check "running job: pid $KEEP not reported as added or deleted" not_added_or_deleted pq "$KEEP"
later "killed job: pid $GONE reported as deleted" has_proc pq "$GONE" deleted
later "new job: pid $NEW reported as added" has_proc pq "$NEW" added
check "diff Q P exits 0" diff_to qp Q P
check "reverse: pid $GONE reported" any_proc qp "$GONE"
check "reverse: pid $NEW reported" any_proc qp "$NEW"
check "diff P P exits 0" diff_to pp P P
check "diff P P has no process records" [ "$(field pp 'len(j["processes"])')" = 0 ]

SHELL_PID="$(wx 'echo "pid=$$"' | sed -n 's/^pid=//p')"
"$W" checkpoint "$SESSION" R >/dev/null
wx 'WAYPOINT_DIFF_TEST_VAR=changed' >/dev/null
"$W" checkpoint "$SESSION" S >/dev/null
check "diff R S exits 0" diff_to rs R S
later "memory-only change (a variable in main's shell, pid $SHELL_PID) reported" \
  any_proc rs "$SHELL_PID"

# ---------------------------------------------------------------------------
say "6. shell: checkpoint T, cd / umask / stty in main, checkpoint U"
# ---------------------------------------------------------------------------
# Read from the CRIU images: the shell's working directory, umask and terminal.
wx 'cd /data; umask 022' >/dev/null
"$W" checkpoint "$SESSION" T >/dev/null
wx 'cd /data/sub; umask 027; stty rows 50 cols 200 echo' >/dev/null
"$W" checkpoint "$SESSION" U >/dev/null
check "diff T U exits 0" diff_to tu T U
check "shell.records is a list" [ "$(field tu 'type(j["shell"]["records"]).__name__')" = list ]
check "cd: cwd /data -> /data/sub" [ "$(shell_change tu cwd)" = '"/data" "/data/sub"' ]
check "umask: 0022 -> 0027" [ "$(shell_change tu umask)" = '"0022" "0027"' ]
check "stty rows cols: tty size 40x120 -> 50x200" \
  [ "$(shell_change tu tty size)" = '{"rows": 40, "cols": 120} {"rows": 50, "cols": 200}' ]
check "stty echo: tty echo false -> true" [ "$(shell_change tu tty echo)" = 'false true' ]
check "idle shell: no foreground record" [ -z "$(shell_change tu foreground)" ]
check "diff U T exits 0" diff_to ut U T
check "reverse: cwd /data/sub -> /data" [ "$(shell_change ut cwd)" = '"/data/sub" "/data"' ]
check "reverse: umask 0027 -> 0022" [ "$(shell_change ut umask)" = '"0027" "0022"' ]
check "diff T T exits 0" diff_to tt T T
check "diff T T has no shell records" [ "$(field tt 'len(j["shell"]["records"])')" = 0 ]
check "a variable set in the shell (R to S): no shell records" [ "$(field rs 'len(j["shell"]["records"])')" = 0 ]

# ---------------------------------------------------------------------------
say "7. cached results"
# ---------------------------------------------------------------------------
# Results live in diffs/<pair>/result-v<N>-<key>.json, key = hash of the target dir.
DDIR="$(echo "$WORK/s/$SESSION"/diffs/A-B-*)"
KEY="$(printf %s /data | sha256sum | cut -c1-16)"
cached() { ls "$DDIR"/result-v*-"$KEY".json >/dev/null 2>&1; }
check "diff A B --dir /data left a cached result" cached
check "a second run exits 0" diff_to ab2 A B --dir /data
check "a second run prints the same result" cmp -s "$OUT/ab.json" "$OUT/ab2.json"
rm -f "$DDIR"/result-v*-"$KEY".json
echo '{"from": "old-format"}' >"$DDIR/result-$KEY.json"
check "diff exits 0 next to a result without a version" diff_to ab3 A B --dir /data
check "a result without a version is not reused" cmp -s "$OUT/ab.json" "$OUT/ab3.json"
# A result cut short, as a crash can leave it.
printf '{"from": "A", "to"' >"$(echo "$DDIR"/result-v*-"$KEY".json)"
check "diff exits 0 next to an incomplete result" diff_to ab5 A B --dir /data
check "an incomplete result is computed again" cmp -s "$OUT/ab.json" "$OUT/ab5.json"

# What a killed diff leaves: its workspace, here with one view still mounted.
rm -f "$DDIR"/result-v*-"$KEY".json
mkdir -p "$DDIR/temp-$KEY"/{left_mount,right_mount}
mount -t overlay overlay -o "ro,lowerdir=$ROOTFS/data:$ROOTFS/bin" "$DDIR/temp-$KEY/left_mount"
check "diff exits 0 after an interrupted run" diff_to ab4 A B --dir /data
check "the result after an interrupted run is the same" cmp -s "$OUT/ab.json" "$OUT/ab4.json"
check "the interrupted run's workspace is removed" [ ! -e "$DDIR/temp-$KEY" ]
check "the interrupted run's mount is gone" no_mount_under "$DDIR"
check "files behind that mount are kept" [ -f "$ROOTFS/data/same.txt" ]

rm -f "$DDIR"/result-v*-"$KEY".json
PIDS=""; DONE=0
for i in 1 2 3 4 5 6; do diff_to "par$i" A B --dir /data & PIDS="$PIDS $!"; done
for p in $PIDS; do wait "$p" && DONE=$((DONE+1)); done
check "6 concurrent diffs of one pair all exit 0" [ "$DONE" -eq 6 ]
SAME=0
for i in 1 2 3 4 5 6; do cmp -s "$OUT/ab.json" "$OUT/par$i.json" && SAME=$((SAME+1)); done
check "6 concurrent diffs print the same result" [ "$SAME" -eq 6 ]

"$W" cleanup "$SESSION" >/dev/null
SESSION=""

COMPLETED=1
say "Summary"
echo "$PASS passed, $FAIL failed, $TODO todo"
[ "$FAIL" -eq 0 ]
