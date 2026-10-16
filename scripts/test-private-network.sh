#!/bin/bash
# Runtime test of private network mode (WAYPOINT_NETWORK=private): each fork
# gets its own network namespace, connected outward by pasta.
#
# Asserts that forks of one checkpoint each own the same port while the parent
# still listens on it, that a fork can bind a port the host already uses, that
# the host never sees fork ports, that forks reach the outside (DNS + HTTP), and
# that snapshot/park/destroy/suspend/cleanup keep or release each fork's
# namespace and pasta as they should.
#
# Requires root, criu, pasta (package passt), a static busybox, and Go unless
# WAYPOINT_BIN and BASH_INIT_BIN are given. Egress checks need the host to reach
# deb.debian.org; set EGRESS=0 to skip them. BRANCHES sets the concurrent fork
# count (default 8). Sessions, registry and rootfs live in a private temp dir.
#
# Usage:  sudo ./scripts/test-private-network.sh
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d /tmp/wpnet.XXXXXX)"
ROOTFS="$WORK/rootfs"
BIN="$WORK/bin"
BRANCHES="${BRANCHES:-8}"
EGRESS="${EGRESS:-1}"
EGRESS_HOST=deb.debian.org
EGRESS_URL="http://$EGRESS_HOST/debian/dists/stable/Release"
SESSION=""
CURRENT_SECTION="startup"
COMPLETED=0
PASS=0; FAIL=0; SKIP=0

export WAYPOINT_SESSIONS_DIR="$WORK/s" WAYPOINT_SESSION_INFO_DIR="$WORK/i"
export WAYPOINT_NETWORK=private
export WAYPOINT_PRESERVE_SESSION_ON_CLEANUP=false

say()  { CURRENT_SECTION="$*"; printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
ok()   { PASS=$((PASS+1)); printf '   \033[0;32mok\033[0m  %s\n' "$*"; }
bad()  { FAIL=$((FAIL+1)); printf '   \033[0;31mFAIL\033[0m %s\n' "$*"; }
skip() { SKIP=$((SKIP+1)); printf '   \033[0;33mskip\033[0m %s\n' "$*"; }
check() { local desc="$1"; shift; if "$@"; then ok "$desc"; else bad "$desc"; fi; }
assert_eq() { [ "$2" = "$3" ] && ok "$1" || bad "$1 — expected '$3', got '$2'"; }

cleanup() {
  if [ "$COMPLETED" -ne 1 ]; then
    printf '\n\033[0;31m!! aborted during "%s"\033[0m\n' "$CURRENT_SECTION"
  fi
  [ -n "$SESSION" ] && "$W" cleanup "$SESSION" --force >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

# fork_field <fork-id> <field>: a field of a fork record from `list --json`.
fork_field() {
  "$W" list "$SESSION" --json | python3 -c '
import json, sys
fork, field = sys.argv[1], sys.argv[2]
for f in json.load(sys.stdin)["forks"]:
    if f["id"] == fork:
        print(f.get(field, ""))' "$1" "$2"
}
netns_of_pid() { stat -L -c %i "/proc/$1/ns/net"; }
netns_of() { netns_of_pid "$(fork_field "$1" pid)"; }
wx() { "$W" exec "$SESSION" "$1" -- "$2"; }
serves() { wx "$1" "wget -T 5 -qO- http://127.0.0.1:$PORT/id" 2>/dev/null || true; }
alive() { [ -n "$1" ] && [ "$1" != 0 ] && kill -0 "$1" 2>/dev/null; }
host_listens() { ss -Hltn "sport = :$1" | grep -q .; }
session_pasta() { pgrep -f -- "--netns /run/waypoint/$SESSION/" || true; }

# ---------------------------------------------------------------------------
say "Environment checks"
# ---------------------------------------------------------------------------
[ "$(id -u)" -eq 0 ] || { echo "must run as root"; exit 1; }
for c in criu pasta busybox ss python3; do
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
mkdir -p "$ROOTFS"/{bin,tmp,proc,sys,root,etc,www}
for b in bash cat sleep; do cp "$(command -v "$b")" "$ROOTFS/bin/$b"; done
for b in "$ROOTFS"/bin/*; do
  { ldd "$b" 2>/dev/null | grep -oE '/[^ ]+\.so[^ ]*' || true; } | while read -r lib; do
    [ -f "$lib" ] || continue
    mkdir -p "$ROOTFS$(dirname "$lib")"
    [ -e "$ROOTFS$lib" ] || cp "$(readlink -f "$lib")" "$ROOTFS$lib"
  done
done
for l in /lib64/ld-linux-x86-64.so.2 /lib/ld-linux-aarch64.so.1; do
  [ -f "$l" ] && [ ! -e "$ROOTFS$l" ] && { mkdir -p "$ROOTFS$(dirname "$l")"; cp "$l" "$ROOTFS$l"; }
done
cp "$(command -v busybox)" "$ROOTFS/bin/busybox"
for a in httpd wget nslookup head grep mkdir; do ln -s busybox "$ROOTFS/bin/$a"; done

PORT=18080
while host_listens "$PORT"; do PORT=$((PORT+1)); done
HOST_PORT="$(ss -Hltn | awk '{print $4}' | grep -oE '(0\.0\.0\.0|127\.0\.0\.1):[0-9]+$' | head -1 | cut -d: -f2 || true)"
echo "fork port: $PORT; a port the host already listens on: ${HOST_PORT:-none}"

if [ "$EGRESS" = 1 ] && ! timeout 10 bash -c "exec 3<>/dev/tcp/$EGRESS_HOST/80" 2>/dev/null; then
  echo "host cannot reach $EGRESS_HOST; egress checks will be skipped"; EGRESS=0
fi
check_egress() {
  if [ "$EGRESS" != 1 ]; then skip "$1 egress (EGRESS=0)"; return; fi
  local out
  out="$(wx "$1" "nslookup $EGRESS_HOST >/dev/null && echo dns-ok; wget -T 10 -qO- $EGRESS_URL | head -1" 2>&1 || true)"
  case "$out" in *dns-ok*) ok "$1 resolves names through pasta's forwarder";; *) bad "$1 DNS failed: $out";; esac
  case "$out" in *Origin:*) ok "$1 fetches over HTTP";; *) bad "$1 HTTP failed: $out";; esac
}

# ---------------------------------------------------------------------------
say "1. init: main runs in its own network namespace"
# ---------------------------------------------------------------------------
OUT="$("$W" init "$ROOTFS" --shell --quiet)"
SESSION="${OUT%%,*}"
echo "session: $SESSION"
assert_eq "session records private network mode" \
  "$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("network",""))' "$WAYPOINT_SESSION_INFO_DIR/$SESSION.json")" private
MAIN_NS="$(netns_of main)"
check "main's network namespace is not the host's" [ "$MAIN_NS" != "$(stat -L -c %i /proc/self/ns/net)" ]
check "main's namespace is pinned under /run/waypoint" [ "$(fork_field main netns)" = "/run/waypoint/$SESSION/main/netns" ]
check "main has a live pasta" alive "$(fork_field main pasta_pid)"
assert_eq "guest resolv.conf points at pasta's forwarder" "$(wx main 'grep ^nameserver /etc/resolv.conf')" "nameserver 169.254.1.1"
check_egress main

# ---------------------------------------------------------------------------
say "2. ports belong to the fork, not the host"
# ---------------------------------------------------------------------------
wx main "echo main > /www/id; httpd -p $PORT -h /www" >/dev/null
sleep 0.3
assert_eq "main serves on port $PORT" "$(serves main)" main
check "the host does not see main's port" bash -c "! ss -Hltn 'sport = :$PORT' | grep -q ."
if [ -n "$HOST_PORT" ]; then
  wx main "mkdir -p /www2; echo mine > /www2/id; httpd -p $HOST_PORT -h /www2" >/dev/null
  sleep 0.3
  assert_eq "main binds port $HOST_PORT, which the host already uses" \
    "$(wx main "wget -T 5 -qO- http://127.0.0.1:$HOST_PORT/id" 2>/dev/null || true)" mine
else
  skip "no host listener found to collide with"
fi

# ---------------------------------------------------------------------------
say "3. fork while main still listens"
# ---------------------------------------------------------------------------
"$W" checkpoint "$SESSION" A >/dev/null
OUT="$("$W" fork "$SESSION" A --n 2 2>&1)" && ok "fork --n 2 restores while main holds port $PORT" || bad "fork --n 2 failed: $OUT"
read -r F1 F2 <<<"$(printf '%s\n' "$OUT" | grep -oE '^fork-[0-9a-f]+' | tr '\n' ' ')"
for f in "$F1" "$F2"; do
  assert_eq "$f inherits main's server" "$(serves "$f")" main
  wx "$f" "echo $f > /www/id" >/dev/null
done
assert_eq "$F1 serves its own content on port $PORT" "$(serves "$F1")" "$F1"
assert_eq "$F2 serves its own content on port $PORT" "$(serves "$F2")" "$F2"
assert_eq "main still serves its own content" "$(serves main)" main
check "forks and main have three distinct namespaces" [ "$(printf '%s\n' "$MAIN_NS" "$(netns_of "$F1")" "$(netns_of "$F2")" | sort -u | wc -l)" -eq 3 ]
check_egress "$F1"

# ---------------------------------------------------------------------------
say "4. snapshot keeps the fork's namespace; park releases it"
# ---------------------------------------------------------------------------
NS_BEFORE="$(netns_of "$F1")"; PASTA_BEFORE="$(fork_field "$F1" pasta_pid)"; PID_BEFORE="$(fork_field "$F1" pid)"
"$W" snapshot "$SESSION" "$F1" B >/dev/null
check "$F1 was restored (new PID)" [ "$(fork_field "$F1" pid)" != "$PID_BEFORE" ]
assert_eq "$F1 keeps its namespace across the snapshot" "$(netns_of "$F1")" "$NS_BEFORE"
assert_eq "$F1 keeps its pasta" "$(fork_field "$F1" pasta_pid)" "$PASTA_BEFORE"
assert_eq "$F1 still serves its own content" "$(serves "$F1")" "$F1"
check_egress "$F1"

PASTA2="$(fork_field "$F2" pasta_pid)"
"$W" snapshot "$SESSION" "$F2" C --park >/dev/null
check "parking $F2 stops its pasta" bash -c "! kill -0 $PASTA2 2>/dev/null"
check "parking $F2 removes its runtime dir" [ ! -e "/run/waypoint/$SESSION/$F2" ]
"$W" fork "$SESSION" C --id revived >/dev/null
assert_eq "a fork of the parked checkpoint serves $F2's content" "$(serves revived)" "$F2"
check "the revived fork has a live pasta" alive "$(fork_field revived pasta_pid)"

PASTA1="$(fork_field "$F1" pasta_pid)"
"$W" destroy "$SESSION" "$F1" >/dev/null
check "destroying $F1 stops its pasta" bash -c "! kill -0 $PASTA1 2>/dev/null"
check "destroying $F1 removes its runtime dir" [ ! -e "/run/waypoint/$SESSION/$F1" ]

# ---------------------------------------------------------------------------
say "5. $BRANCHES concurrent forks"
# ---------------------------------------------------------------------------
t0="$(date +%s%N)"
OUT="$("$W" fork "$SESSION" A --n "$BRANCHES" 2>&1)" && ok "fork --n $BRANCHES ($(( ($(date +%s%N)-t0)/1000000 )) ms)" || bad "fork --n $BRANCHES failed: $OUT"
FORKS="$(printf '%s\n' "$OUT" | grep -oE '^fork-[0-9a-f]+' || true)"
own=0; nss=""
for f in $FORKS; do
  wx "$f" "echo $f > /www/id" >/dev/null
  [ "$(serves "$f")" = "$f" ] && own=$((own+1))
  nss="$nss $(netns_of "$f")"
done
assert_eq "each concurrent fork serves its own content" "$own" "$BRANCHES"
assert_eq "each concurrent fork has its own namespace" "$(printf '%s\n' $nss | sort -u | wc -l)" "$BRANCHES"

# ---------------------------------------------------------------------------
say "6. suspend and cleanup release everything"
# ---------------------------------------------------------------------------
"$W" suspend "$SESSION" >/dev/null
assert_eq "suspend leaves no pasta for the session" "$(session_pasta)" ""
check "suspend removes the session's runtime dir" [ ! -e "/run/waypoint/$SESSION" ]
assert_eq "suspend leaves no mounts" "$(grep -c " $WORK/" /proc/self/mountinfo || true)" 0
"$W" fork "$SESSION" A --id again >/dev/null
assert_eq "a fork after suspend serves the checkpoint's content" "$(serves again)" main
check_egress again

"$W" cleanup "$SESSION" >/dev/null
assert_eq "cleanup leaves no pasta for the session" "$(session_pasta)" ""
check "cleanup removes the session's runtime dir" [ ! -e "/run/waypoint/$SESSION" ]
assert_eq "cleanup leaves no mounts" "$(grep -c " $WORK/" /proc/self/mountinfo || true)" 0
check "cleanup removes the session dir" [ ! -e "$WAYPOINT_SESSIONS_DIR/$SESSION" ]
SESSION=""

COMPLETED=1
say "Summary"
echo "$PASS passed, $FAIL failed, $SKIP skipped"
[ "$FAIL" -eq 0 ]
