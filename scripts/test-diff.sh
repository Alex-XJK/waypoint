#!/bin/bash
# Runtime test of `waypoint diff` and `waypoint state` on a busybox+bash rootfs.
#
#  1. Filesystem changes between two checkpoints of main: content, add, delete,
#     mode on a file and a directory, an empty directory, a removed directory
#     (whiteout), a directory removed and recreated (opaque), a directory
#     replaced by a file, a symlink retarget, and two changes that leave the
#     content as it was (touch, rewrite with the same bytes), which must not
#     be reported. Nothing of Waypoint's own (/.waypoint, the session dir).
#  2. Process changes in the same pair: the shell's cwd and a new fd, a new
#     background process and a listening daemon, an exported variable in the
#     children's environment, and `state` for one checkpoint.
#  3. Two branches from one checkpoint: only what they did after it.
#  4. Across sessions: equal originals, and an original with one file changed
#     (base_only records).
#  5. With incremental dumps: argv and environment read through the parent
#     image set.
#  7. --content: unified line diffs of changed text files, checked by applying
#     each one with patch(1); binary and too-large files are omitted.
#     --word-diff: the same records with changed words marked inline.
#
# Requires root, criu (Waypoint's build), a static busybox, python3 and Go
# unless WAYPOINT_BIN and BASH_INIT_BIN are given. Everything lives in a
# private temp dir, removed at the end; KEEP_OUT=DIR keeps the JSON outputs.
#
# Usage:  sudo ./scripts/test-diff.sh
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d /tmp/wpdiff.XXXXXX)"
OUT="$WORK/out"
SESSIONS=()
PASS=0; FAIL=0
PORT=$((40000 + RANDOM % 20000))

export WAYPOINT_SESSIONS_DIR="$WORK/s" WAYPOINT_SESSION_INFO_DIR="$WORK/i"
export WAYPOINT_NETWORK=shared WAYPOINT_PRESERVE_SESSION_ON_CLEANUP=false

say()  { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
ok()   { PASS=$((PASS+1)); printf '   \033[0;32mok\033[0m  %s\n' "$*"; }
bad()  { FAIL=$((FAIL+1)); printf '   \033[0;31mFAIL\033[0m %s\n' "$*"; }
check() { local desc="$1"; shift; if "$@"; then ok "$desc"; else bad "$desc"; fi; }

cleanup() {
  [ -n "${KEEP_OUT:-}" ] && cp -r "$OUT" "$KEEP_OUT" 2>/dev/null || true
  for s in "${SESSIONS[@]}"; do "$W" cleanup "$s" --force >/dev/null 2>&1 || true; done
  awk -v w="$WORK" 'index($5, w "/") == 1 {print $5}' /proc/self/mountinfo |
    sort -r | while read -r mp; do umount -l "$mp" 2>/dev/null || true; done
  rm -rf --one-file-system "$WORK"
}
trap cleanup EXIT

[ "$(id -u)" -eq 0 ] || { echo "must run as root"; exit 1; }
for c in criu busybox python3 patch; do command -v "$c" >/dev/null || { echo "missing $c"; exit 1; }; done

if [ -n "${WAYPOINT_BIN:-}" ] && [ -n "${BASH_INIT_BIN:-}" ]; then
  W="$WAYPOINT_BIN"; export WAYPOINT_BASH_INIT_SRC="$BASH_INIT_BIN"
else
  GO="$(command -v go || echo /usr/local/go/bin/go)"
  mkdir -p "$WORK/bin"
  ( cd "$REPO"
    env GOCACHE="${GOCACHE:-/tmp/waypoint-go-cache}" "$GO" build -o "$WORK/bin/waypoint" ./cmd/waypoint
    env GOCACHE="${GOCACHE:-/tmp/waypoint-go-cache}" CGO_ENABLED=0 "$GO" build -o "$WORK/bin/bash_init" ./cmd/bash-init )
  W="$WORK/bin/waypoint"; export WAYPOINT_BASH_INIT_SRC="$WORK/bin/bash_init"
fi
mkdir -p "$OUT"

# make_rootfs DIR: bash with its libraries, busybox applets, and /data.
make_rootfs() {
  local R="$1"
  mkdir -p "$R"/{bin,tmp,proc,sys,root,etc,www}
  cp "$(command -v bash)" "$R/bin/bash"
  ldd "$R/bin/bash" | grep -oE '/[^ ]+\.so[^ ]*' | while read -r lib; do
    [ -f "$lib" ] || continue
    mkdir -p "$R$(dirname "$lib")"; cp "$(readlink -f "$lib")" "$R$lib"
  done
  cp "$(command -v busybox)" "$R/bin/busybox"
  for a in sh cat true chmod ln mkdir rm rmdir touch sleep httpd printf cp; do ln -s busybox "$R/bin/$a"; done
  echo hello >"$R/www/index.html"
  local D="$R/data"
  mkdir -p "$D"/{gone/sub,redo,todir,dirperm}
  echo same >"$D/same.txt"; echo v1 >"$D/mod.txt"; echo d >"$D/del.txt"; echo c >"$D/chmod.txt"
  echo t >"$D/touched.txt"; echo r >"$D/rewritten.txt"
  echo g >"$D/gone/a"; echo g >"$D/gone/sub/b"; echo old >"$D/redo/old"; echo x >"$D/todir/x"
  ln -s same.txt "$D/link"
  # For --content: code.py becomes code.new (two hunks, a "<" that must stay
  # unescaped, no final newline); blob.bin is binary.
  for i in $(seq 1 40); do echo "line $i"; done >"$D/code.py"
  { for i in $(seq 1 40); do
      case $i in 5) echo "if a < b: line 5" ;; 30) ;; 35) echo "line 35"; echo inserted ;; *) echo "line $i" ;; esac
    done; printf 'tail'; } >"$D/code.new"
  printf 'a\000b' >"$D/blob.bin"
}

new_session() { # new_session ROOTFS -> prints session id
  local out; out="$("$W" init "$1" --shell --quiet)"; echo "${out%%,*}"
}
wx() { "$W" exec "$1" "$2" -- "$3" >/dev/null; }

# q NAME EXPR: evaluates a python expression on $OUT/NAME.json (as j).
q() { python3 -c 'import json,sys; j=json.load(open(sys.argv[1])); print(eval(sys.argv[2]))' "$OUT/$1.json" "$2"; }
rec() { # rec NAME PATH -> "change fields" for a filesystem record, or nothing
  python3 - "$OUT/$1.json" "$2" <<'EOF'
import json, sys
for r in json.load(open(sys.argv[1]))["filesystem"]["records"]:
    if r["path"] == sys.argv[2]:
        print(r["change"], ",".join(r.get("fields") or []))
EOF
}
is_rec() { [ "$(rec "$1" "$2")" = "$3" ]; }
no_rec() { [ -z "$(rec "$1" "$2")" ]; }
true_() { [ "$(q "$1" "$2")" = "True" ]; }

ROOT1="$WORK/rootfs1"; make_rootfs "$ROOT1"

say "1. filesystem changes between two checkpoints of main"
S1="$(new_session "$ROOT1")"; SESSIONS+=("$S1"); echo "session $S1"
wx "$S1" main 'true'
"$W" checkpoint "$S1" A >/dev/null
wx "$S1" main 'echo v2 >/data/mod.txt; echo n >/data/new.txt; rm /data/del.txt; chmod 600 /data/chmod.txt'
wx "$S1" main 'chmod 700 /data/dirperm; mkdir /data/emptydir; rm -rf /data/gone'
wx "$S1" main 'rm -rf /data/redo; mkdir /data/redo; echo new >/data/redo/new'
wx "$S1" main 'rm -rf /data/todir; echo f >/data/todir; ln -sfn mod.txt /data/link'
wx "$S1" main 'touch /data/touched.txt; echo r >/data/rewritten.txt'
wx "$S1" main 'cp /data/code.new /data/code.py; printf "a\\000c" >/data/blob.bin'
# process side
wx "$S1" main 'cd /data; export FOO=bar; exec 5>/data/log.txt; echo line >&5'
wx "$S1" main 'sleep 100000 &'
wx "$S1" main "httpd -p 127.0.0.1:$PORT -h /www"
wx "$S1" main 'true'
"$W" checkpoint "$S1" B >/dev/null
start=$(date +%s%N)
"$W" diff "$S1" A B >"$OUT/ab.json"
echo "   diff A B took $(( ($(date +%s%N) - start) / 1000000 )) ms"

check "schema and refs" true_ ab 'j["schema"]=="waypoint.diff/v1" and j["from"]["checkpoint"]=="A" and j["to"]["checkpoint"]=="B"'
check "base: shared layers [A], unshared from [] and to [B]" true_ ab 'j["filesystem"]["base"]["kind"]=="shared_layers" and j["filesystem"]["base"]["shared_layers"]==["A"] and j["filesystem"]["from_layers"]==[] and j["filesystem"]["to_layers"]==["B"]'
check "modified content"            is_rec ab /data/mod.txt "modified content"
check "added file"                  is_rec ab /data/new.txt "added "
check "deleted file"                is_rec ab /data/del.txt "deleted "
check "file mode"                   is_rec ab /data/chmod.txt "modified mode"
check "directory mode"              is_rec ab /data/dirperm "modified mode"
check "empty directory added"       is_rec ab /data/emptydir "added "
check "removed directory"           is_rec ab /data/gone "deleted "
check "file under removed directory" is_rec ab /data/gone/sub/b "deleted "
check "recreated dir: old file gone" is_rec ab /data/redo/old "deleted "
check "recreated dir: new file"     is_rec ab /data/redo/new "added "
check "recreated dir itself unchanged" no_rec ab /data/redo
check "directory replaced by a file" is_rec ab /data/todir "modified type"
check "file of replaced directory"  is_rec ab /data/todir/x "deleted "
check "symlink target"              is_rec ab /data/link "modified target"
check "touch only: not reported"    no_rec ab /data/touched.txt
check "same bytes rewritten: not reported" no_rec ab /data/rewritten.txt
check "unchanged file: not reported" no_rec ab /data/same.txt
check "nothing under /.waypoint or the session dir" true_ ab 'not any(r["path"].startswith("/.waypoint") or r["path"].startswith("'"$WORK"'") for r in j["filesystem"]["records"])'
check "sha256 on a modified file" true_ ab '[r for r in j["filesystem"]["records"] if r["path"]=="/data/mod.txt"][0]["to"]["sha256"]=="'"$(printf 'v2\n' | sha256sum | cut -d' ' -f1)"'"'
check "layer of the new version is B" true_ ab '[r for r in j["filesystem"]["records"] if r["path"]=="/data/mod.txt"][0]["to"]["layer"]=="B"'

say "2. process changes"
P='j["processes"]'
check "bash: cwd / -> /data" true_ ab "any(c['key']=='[waypoint] > /bin/bash --noprofile --noediting' and {'field':'cwd','from':'/','to':'/data'} in c.get('fields',[]) for c in $P['changed'])"
check "bash: fd 5 on /data/log.txt" true_ ab "any(fd['fd']==5 and fd['target']=='/data/log.txt' and fd['access']=='w' for c in $P['changed'] if c.get('fds') for fd in c['fds']['added'])"
check "sleep added with argv and cwd" true_ ab "any(p['argv']==['sleep','100000'] and p['cwd']=='/data' for p in $P['added'])"
check "httpd added with its listening socket" true_ ab "any(p['argv'][0]=='httpd' and any(fd['kind']=='tcp' and fd['target']=='127.0.0.1:$PORT listen' for fd in p['fds']) for p in $P['added'])"
check "socket diff lists the listener" true_ ab "any(s['proto']=='tcp' and s['local']=='127.0.0.1:$PORT' and s['state']=='listen' for s in $P['sockets']['added'])"
check "children inherited FOO=bar" true_ ab "all('FOO=bar' in p['env'] for p in $P['added'])"
check "no internal fds or processes" true_ ab "not any(fd.get('internal') for p in $P['added'] for fd in p['fds']) and not any(p.get('internal') for p in $P['added']+$P['removed'])"
check "nothing unavailable" true_ ab "not $P.get('unavailable')"
"$W" state "$S1" B >"$OUT/stateB.json"
check "state: root is internal [waypoint]" true_ stateB 'j["processes"][0]["internal"] and j["processes"][0]["key"]=="[waypoint]"'
check "state: listener owned by httpd" true_ stateB "any(s['local']=='127.0.0.1:$PORT' and 'httpd' in s['owners'][0] for s in j['sockets'])"
"$W" diff "$S1" A A >"$OUT/aa.json"
check "a checkpoint against itself is empty" true_ aa 'j["filesystem"]["records"]==[] and j["processes"]["added"]==[] and j["processes"]["changed"]==[]'
"$W" diff "$S1" A B --fs-only >"$OUT/fsonly.json"
check "--fs-only has no process part" true_ fsonly '"processes" not in j and "filesystem" in j'

say "3. two branches from one checkpoint"
"$W" fork "$S1" A --id f1 >/dev/null
wx "$S1" f1 'echo branch >/data/mod.txt; echo only-c >/data/c.txt'
"$W" snapshot "$S1" f1 C >/dev/null
"$W" diff "$S1" B C --fs-only >"$OUT/bc.json"
check "base shared at A" true_ bc 'j["filesystem"]["base"]["shared_layers"]==["A"] and j["filesystem"]["from_layers"]==["B"] and j["filesystem"]["to_layers"]==["C"]'
check "both changed mod.txt differently" is_rec bc /data/mod.txt "modified content"
check "only C has c.txt"            is_rec bc /data/c.txt "added "
check "only B has new.txt"          is_rec bc /data/new.txt "deleted "
check "untouched by both: not reported" no_rec bc /data/same.txt

say "4. across sessions"
S2="$(new_session "$ROOT1")"; SESSIONS+=("$S2")
wx "$S2" main 'echo s2 >/data/s2.txt'
"$W" checkpoint "$S2" A >/dev/null
"$W" diff "$S1" A "$S2:A" >"$OUT/x12.json"
check "equal originals" true_ x12 'j["filesystem"]["base"]["kind"]=="equal_originals"'
check "the other session's file is added" is_rec x12 /data/s2.txt "added "
check "no other file records" true_ x12 '[r["path"] for r in j["filesystem"]["records"]]==["/data/s2.txt"]'
check "processes: same shell, no change" true_ x12 'j["processes"]["added"]==[] and j["processes"]["removed"]==[] and j["processes"]["changed"]==[]'
ROOT3="$WORK/rootfs3"; make_rootfs "$ROOT3"; echo different >"$ROOT3/data/same.txt"
S3="$(new_session "$ROOT3")"; SESSIONS+=("$S3")
wx "$S3" main 'true'
"$W" checkpoint "$S3" A >/dev/null
"$W" diff "$S1" A "$S3:A" --fs-only >"$OUT/x13.json"
check "different originals" true_ x13 'j["filesystem"]["base"]["kind"]=="different_originals"'
check "base-only record for same.txt" true_ x13 'any(r["path"]=="/data/same.txt" and r["base_only"] and r["fields"]==["content"] for r in j["filesystem"]["records"])'
check "nothing else differs" true_ x13 'len(j["filesystem"]["records"])==1'

say "5. incremental dumps: pages through the parent image set"
S4="$(WAYPOINT_INCREMENTAL_DUMPS=true new_session "$ROOT1")"; SESSIONS+=("$S4")
export WAYPOINT_INCREMENTAL_DUMPS=true
# busybox sleep returns when its tree is dumped, so the long-lived process
# here is a daemon.
wx "$S4" main "httpd -p 127.0.0.1:$((PORT + 1)) -h /www"
"$W" checkpoint "$S4" A >/dev/null
"$W" fork "$S4" A --id g1 >/dev/null
wx "$S4" g1 'cd /data'
"$W" snapshot "$S4" g1 B >/dev/null
unset WAYPOINT_INCREMENTAL_DUMPS
check "B has a parent image set" test -e "$WAYPOINT_SESSIONS_DIR/$S4/checkpoints/B/criu/parent"
"$W" state "$S4" B >"$OUT/inc.json"
check "argv read through the chain" true_ inc "any(p['argv']==['httpd','-p','127.0.0.1:$((PORT + 1))','-h','/www'] for p in j['processes'])"
HPID="$(q inc "[p['pid'] for p in j['processes'] if p['argv'][:1]==['httpd']][0]")"
if command -v crit >/dev/null; then
  check "httpd's pages in B point to the parent" bash -c "crit decode -i '$WAYPOINT_SESSIONS_DIR/$S4/checkpoints/B/criu/pagemap-$HPID.img' --pretty | grep -q PE_PARENT"
fi
check "nothing unavailable" true_ inc 'not j.get("unavailable")'
"$W" diff "$S4" A B --proc-only >"$OUT/incd.json"
check "only the cwd changed" true_ incd "len(j['processes']['changed'])==1 and j['processes']['changed'][0]['fields']==[{'field':'cwd','from':'/','to':'/data'}] and j['processes']['added']==[]"

say "6. argument errors"
check "unknown flag refused" bash -c "! '$W' diff '$S1' A B --bogus >/dev/null 2>&1"
check "missing checkpoint refused" bash -c "! '$W' diff '$S1' A nope >/dev/null 2>&1"
check "both --fs-only and --proc-only refused" bash -c "! '$W' diff '$S1' A B --fs-only --proc-only >/dev/null 2>&1"

say "7. --content: line diffs"
"$W" diff "$S1" A B --fs-only --content >"$OUT/content.json"
# applies NAME's patch for PATH to FROM (or nothing) and compares with WANT
patch_gives() {
  local name="$1" path="$2" from="$3" want="$4" t="$WORK/patchtest"
  rm -rf "$t"; mkdir -p "$t"
  python3 -c 'import json,sys
for r in json.load(open(sys.argv[1]))["filesystem"]["records"]:
    if r["path"]==sys.argv[2]: sys.stdout.write(r["patch"])' "$OUT/$name.json" "$path" >"$t/p"
  [ -s "$t/p" ] || return 1
  if [ -n "$from" ]; then cp "$from" "$t/f"; else : >"$t/f"; fi
  patch -s -o "$t/out" "$t/f" <"$t/p" >/dev/null && cmp -s "$t/out" "$want"
}
check "code.py: patch(1) turns A's file into B's" patch_gives content /data/code.py "$ROOT1/data/code.py" "$ROOT1/data/code.new"
check "code.py: two hunks" true_ content '[r for r in j["filesystem"]["records"] if r["path"]=="/data/code.py"][0]["patch"].count("\n@@ ")==2'
check "\"<\" is not escaped in the JSON" grep -q 'if a < b: line 5' "$OUT/content.json"
printf 'v2\n' >"$WORK/mod.want"
check "mod.txt: patch(1) gives v2" patch_gives content /data/mod.txt "$ROOT1/data/mod.txt" "$WORK/mod.want"
check "added file: patch from /dev/null" true_ content '[r for r in j["filesystem"]["records"] if r["path"]=="/data/new.txt"][0]["patch"]=="--- /dev/null\n+++ b/data/new.txt\n@@ -0,0 +1 @@\n+n\n"'
check "deleted file: patch to /dev/null" true_ content '[r for r in j["filesystem"]["records"] if r["path"]=="/data/del.txt"][0]["patch"]=="--- a/data/del.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-d\n"'
check "directory replaced by a file: patch adds it" true_ content '[r for r in j["filesystem"]["records"] if r["path"]=="/data/todir"][0]["patch"].startswith("--- /dev/null\n")'
check "binary file omitted" true_ content '[r for r in j["filesystem"]["records"] if r["path"]=="/data/blob.bin"][0]["patch_omitted"]=="binary"'
check "mode-only change has no patch" true_ content '[r for r in j["filesystem"]["records"] if r["path"]=="/data/chmod.txt"][0].keys().isdisjoint({"patch","patch_omitted"})'
check "without --content: no patches" true_ fsonly 'not any("patch" in r or "patch_omitted" in r for r in j["filesystem"]["records"])'
"$W" diff "$S1" A B --fs-only --content --content-max=100 >"$OUT/small.json"
check "--content-max: code.py too large" true_ small '[r for r in j["filesystem"]["records"] if r["path"]=="/data/code.py"][0]["patch_omitted"]=="too_large"'
check "--content-max: small files still diffed" true_ small '"patch" in [r for r in j["filesystem"]["records"] if r["path"]=="/data/mod.txt"][0]'
check "--content with --proc-only refused" bash -c "! '$W' diff '$S1' A B --proc-only --content >/dev/null 2>&1"
check "--content-max without --content refused" bash -c "! '$W' diff '$S1' A B --content-max=5 >/dev/null 2>&1"
check "--content-max=0 refused" bash -c "! '$W' diff '$S1' A B --content --content-max=0 >/dev/null 2>&1"
"$W" diff "$S1" A B --fs-only --word-diff >"$OUT/words.json"
check "--word-diff: patch_format word" true_ words 'j["filesystem"]["patch_format"]=="word"'
check "--word-diff: mod.txt marked inline" true_ words '[r for r in j["filesystem"]["records"] if r["path"]=="/data/mod.txt"][0]["patch"]=="--- a/data/mod.txt\n+++ b/data/mod.txt\n@@ -1 +1 @@\n[-v1-]{+v2+}\n"'
check "--word-diff: code.py words marked, no -/+ lines" true_ words '(lambda p: "{+if a < b: +}line 5\n" in p and "[-line 30\n-]line 31\n" in p and "{+inserted\n+}line 36\n" in p and "\n{+tail+}\n\\ No newline at end of file\n" in p and "\n-line" not in p)([r for r in j["filesystem"]["records"] if r["path"]=="/data/code.py"][0]["patch"])'
check "--word-diff: binary still omitted" true_ words '[r for r in j["filesystem"]["records"] if r["path"]=="/data/blob.bin"][0]["patch_omitted"]=="binary"'
check "--content: patch_format unified" true_ content 'j["filesystem"]["patch_format"]=="unified"'
check "--word-diff with --proc-only refused" bash -c "! '$W' diff '$S1' A B --proc-only --word-diff >/dev/null 2>&1"

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
