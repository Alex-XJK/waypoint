#!/bin/bash
# Runtime test of fabric attachment (--attach, private network mode): forks of
# two sessions attached to one fabric hold TCP connections that survive
# snapshot, fork into other fabrics, and a second-generation fork.
#
# The test plays the fabric owner: each "replica" is a network namespace with
# bridge br0, and its barrier is an nft bridge-family drop rule. A web session
# (10.89.0.2) and a db session (10.89.0.3) are attached to each replica with
# the same addresses. The guest workload (scripts/fabric-peer) keeps a
# request/response connection and a ~12 MB/s bulk stream open and checks both
# for gaps; nothing reconnects, so a broken connection stays visible.
#
# Also asserts refusals (shared mode, --n with --attach, bad addresses, a
# fabric without br0), that destroy/park/suspend/cleanup remove a fork's end of
# the fabric, and that nothing is left behind.
#
# Requires root, criu, pasta (package passt), nft, ip (iproute2), unshare, and
# Go unless WAYPOINT_BIN, BASH_INIT_BIN and FABRIC_PEER_BIN are given. K sets
# the replica count (default 4, at least 3). If SELinux confines pasta (see
# `./setup check`), an unconfined copy is used. Sessions, registry, fabrics and
# rootfs are private to the test.
#
# Usage:  sudo ./scripts/test-fabric.sh
set -uo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d /tmp/wpfab.XXXXXX)"
TAG="$(basename "$WORK" | cut -d. -f2)"
ROOTFS="$WORK/rootfs"
BIN="$WORK/bin"
K="${K:-4}"
IP_WEB=10.89.0.2/24 IP_DB=10.89.0.3/24 DB_ADDR=10.89.0.3
WEB="" DB=""
FABRICS=()
CURRENT_SECTION="startup"
COMPLETED=0
PASS=0; FAIL=0

export WAYPOINT_SESSIONS_DIR="$WORK/s" WAYPOINT_SESSION_INFO_DIR="$WORK/i"
export WAYPOINT_NETWORK=private
export WAYPOINT_PRESERVE_SESSION_ON_CLEANUP=false

say()  { CURRENT_SECTION="$*"; printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
ok()   { PASS=$((PASS+1)); printf '   \033[0;32mok\033[0m  %s\n' "$*"; }
bad()  { FAIL=$((FAIL+1)); printf '   \033[0;31mFAIL\033[0m %s\n' "$*"; }
check() { local desc="$1"; shift; if "$@"; then ok "$desc"; else bad "$desc"; fi; }
ms()   { date +%s%3N; }
die()  { bad "$*"; exit 1; }

leftovers() {
  local ns pasta mnt proc pins
  ns="$(ip netns list | grep -c "wpf$TAG" || true)"
  pasta="$(pgrep -fc -- "--netns /run/waypoint/(${WEB:-x}|${DB:-x})/" || true)"
  mnt="$(grep -c "$WORK" /proc/self/mountinfo || true)"
  proc="$(for p in /proc/[0-9]*; do readlink "$p/root" 2>/dev/null; done | grep -c "$WORK" || true)"
  pins="$(ls -d /run/waypoint/"${WEB:-x}" /run/waypoint/"${DB:-x}" 2>/dev/null | wc -l)"
  echo "fabrics=$ns pasta=$pasta mounts=$mnt procs=$proc pins=$pins"
}

cleanup() {
  if [ "$COMPLETED" -ne 1 ]; then
    printf '\n\033[0;31m!! aborted during "%s"\033[0m\n' "$CURRENT_SECTION"
  fi
  for s in $WEB $DB; do "$W" cleanup "$s" --force >/dev/null 2>&1 || true; done
  for f in "${FABRICS[@]}"; do ip netns del "$f" 2>/dev/null; done
  rm -rf "$WORK"
  printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
  [ "$FAIL" -eq 0 ] && [ "$COMPLETED" -eq 1 ]
}
trap cleanup EXIT

# --- fabric owner -----------------------------------------------------------
fab()      { echo "wpf$TAG-$1"; }
fabns()    { echo "/run/netns/$(fab "$1")"; }
make_fabric() {
  local f; f="$(fab "$1")"; FABRICS+=("$f")
  ip netns add "$f" && ip -n "$f" link set lo up &&
    ip -n "$f" link add br0 type bridge && ip -n "$f" link set br0 up
}
barrier_on()  {
  ip netns exec "$(fab "$1")" nft add table bridge wpbar &&
    ip netns exec "$(fab "$1")" nft add chain bridge wpbar gate '{ type filter hook forward priority 0; policy drop; }'
}
barrier_off() { ip netns exec "$(fab "$1")" nft delete table bridge wpbar; }
# fabric_ports <replica>: the fork ends on the replica's bridge
fabric_ports() { ip -n "$(fab "$1")" -o link show master br0 2>/dev/null | wc -l; }
# wait_ports <replica> <n>: wait up to 3s for the bridge to have n ports
# (a released namespace's veth goes away asynchronously)
wait_ports() {
  local i; for i in $(seq 30); do [ "$(fabric_ports "$1")" = "$2" ] && return 0; sleep 0.1; done
  return 1
}

# --- guest workload -----------------------------------------------------------
x()     { "$W" exec "$1" "$2" -- "$3"; }
field() { tr ' ' '\n' <<<"$1" | sed -n "s/^$2=//p"; }
cli()   { x "$WEB" "$1" 'cat /tmp/cli.status'; }
srv()   { x "$DB" "$1" 'cat /tmp/srv.status'; }
bulk()  { x "$DB" "$1" 'cat /tmp/bulk.status'; }
start_workload() {
  local env=GODEBUG=multipathtcp=0 # criu cannot dump Go's default MPTCP sockets
  x "$DB" "$1" "echo db-$1 > /id; $env /bin/fabric-peer server $DB_ADDR:9000 >/tmp/srv.log 2>&1 </dev/null &
    $env /bin/fabric-peer bulk-server $DB_ADDR:9001 >/tmp/bulk.log 2>&1 </dev/null &" >/dev/null
  sleep 0.3
  x "$WEB" "$1" "$env /bin/fabric-peer client $DB_ADDR:9000 >/tmp/cli.log 2>&1 </dev/null &
    $env /bin/fabric-peer bulk-client $DB_ADDR:9001 >/tmp/bulkc.log 2>&1 </dev/null &" >/dev/null
  local i
  for i in $(seq 100); do
    [ "$(field "$(cli "$1" 2>/dev/null)" state)" = ok ] && [ -n "$(field "$(bulk "$1" 2>/dev/null)" recv)" ] && return 0
    sleep 0.1
  done
  return 1
}
# wait_flowing <fork>: wait up to 60s for both streams to advance (traffic
# can stall for seconds after a barrier while TCP retransmissions back off)
wait_flowing() {
  local c0 b0 t0; c0="$(field "$(cli "$1")" last)"; b0="$(field "$(bulk "$1")" recv)"; t0="$(ms)"
  while [ $(( $(ms) - t0 )) -lt 60000 ]; do
    if [ "$(field "$(cli "$1")" last)" != "$c0" ] && [ "$(field "$(bulk "$1")" recv)" != "$b0" ]; then
      echo $(( $(ms) - t0 )); return 0
    fi
    sleep 0.1
  done
  return 1
}
# check_replica <fork>: both connections alive, intact, never reconnected,
# and answered by this fork's own db
check_replica() {
  local c s b
  c="$(cli "$1")"; s="$(srv "$1")"; b="$(bulk "$1")"
  [ "$(field "$c" state)" = ok ] && [ "$(field "$c" errors)" = 0 ] &&
    ok "$1: request/response connection alive" || bad "$1: client $c"
  [ "$(field "$c" peer)" = "db-$1" ] && ok "$1: answered by db-$1" || bad "$1: answered by '$(field "$c" peer)'"
  [ "$(field "$s" conns)" = 1 ] && [ "$(field "$s" gaps)" = 0 ] &&
    ok "$1: request stream intact on its first connection" || bad "$1: server $s"
  [ "$(field "$b" conns)" = 1 ] && [ "$(field "$b" gaps)" = 0 ] && [ "$(field "$b" state)" = ok ] &&
    ok "$1: bulk stream intact on its first connection" || bad "$1: bulk $b"
}
# fork_pair <checkpoint> <fork>: fork both sessions onto fabric <fork>
fork_pair() {
  "$W" fork "$WEB" "$1" --id "$2" --attach "$(fabns "$2")=$IP_WEB" >"$WORK/fork-web-$2.log" 2>&1 & local p1=$!
  "$W" fork "$DB" "$1" --id "$2" --attach "$(fabns "$2")=$IP_DB" >"$WORK/fork-db-$2.log" 2>&1 & local p2=$!
  local rc=0; wait $p1 || rc=1; wait $p2 || rc=1
  [ $rc = 0 ] || cat "$WORK/fork-web-$2.log" "$WORK/fork-db-$2.log"
  return $rc
}
# snapshot_pair <fork> <checkpoint> [--park]: barrier, snapshot both, release
snapshot_pair() {
  local f="$1" c="$2"; shift 2
  barrier_on "$f" || return 1
  "$W" snapshot "$WEB" "$f" "$c" "$@" >"$WORK/snap-web-$c.log" 2>&1 & local p1=$!
  "$W" snapshot "$DB" "$f" "$c" "$@" >"$WORK/snap-db-$c.log" 2>&1 & local p2=$!
  local rc=0; wait $p1 || rc=1; wait $p2 || rc=1
  barrier_off "$f"
  [ $rc = 0 ] || cat "$WORK/snap-web-$c.log" "$WORK/snap-db-$c.log"
  return $rc
}

# ---------------------------------------------------------------------------
say "Environment checks"
# ---------------------------------------------------------------------------
[ "$(id -u)" -eq 0 ] || { echo "must run as root"; exit 1; }
[ "$K" -ge 3 ] || { echo "K must be at least 3 (sections 6 and 7 use replicas r2 and r3)"; exit 1; }
for c in criu pasta nft ip unshare; do
  command -v "$c" >/dev/null || { echo "missing required command: $c"; exit 1; }
done

# ---------------------------------------------------------------------------
say "Build waypoint, bash_init, fabric-peer"
# ---------------------------------------------------------------------------
mkdir -p "$BIN"
if [ -n "${WAYPOINT_BIN:-}" ] && [ -n "${BASH_INIT_BIN:-}" ] && [ -n "${FABRIC_PEER_BIN:-}" ]; then
  cp "$WAYPOINT_BIN" "$BIN/waypoint"; cp "$BASH_INIT_BIN" "$BIN/bash_init"; cp "$FABRIC_PEER_BIN" "$BIN/fabric-peer"
else
  GO="$(command -v go || true)"
  for cand in /usr/local/go/bin/go "$HOME/go/bin/go" "/home/${SUDO_USER:-}/go/bin/go"; do
    [ -n "$GO" ] && break
    [ -x "$cand" ] && GO="$cand"
  done
  [ -n "$GO" ] || { echo "go toolchain not found; set WAYPOINT_BIN, BASH_INIT_BIN and FABRIC_PEER_BIN"; exit 1; }
  ( cd "$REPO"
    export GOCACHE="${GOCACHE:-/tmp/waypoint-go-cache}"
    "$GO" build -o "$BIN/waypoint" ./cmd/waypoint &&
      CGO_ENABLED=0 "$GO" build -o "$BIN/bash_init" ./cmd/bash-init &&
      CGO_ENABLED=0 "$GO" build -o "$BIN/fabric-peer" ./scripts/fabric-peer ) || die "build failed"
fi
W="$BIN/waypoint"; export WAYPOINT_BASH_INIT_SRC="$BIN/bash_init"
if [ "$(cat /sys/fs/selinux/enforce 2>/dev/null)" = 1 ] &&
    ls -Z "$(command -v pasta)" 2>/dev/null | grep -q pasta_exec_t; then
  echo "SELinux confines $(command -v pasta); using an unconfined copy"
  cp "$(command -v pasta)" "$BIN/pasta"; export PATH="$BIN:$PATH"
fi

# ---------------------------------------------------------------------------
say "Assemble a rootfs"
# ---------------------------------------------------------------------------
mkdir -p "$ROOTFS"/{bin,tmp,proc,sys,root,etc}
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
cp "$BIN/fabric-peer" "$ROOTFS/bin/"

# ---------------------------------------------------------------------------
say "1. refusals leave nothing behind"
# ---------------------------------------------------------------------------
make_fabric main || die "create fabric"
make_fabric nobridge || die "create fabric"
ip -n "$(fab nobridge)" link del br0
refuse() { local desc="$1"; shift; if "$@" >/dev/null 2>&1; then bad "$desc was accepted"; else ok "$desc is refused"; fi; }
refuse "--attach without --shell" "$W" init "$ROOTFS" --attach "$(fabns main)=$IP_WEB"
refuse "--attach in shared mode" env WAYPOINT_NETWORK=shared "$W" init "$ROOTFS" --shell --attach "$(fabns main)=$IP_WEB"
refuse "a network address" "$W" init "$ROOTFS" --shell --attach "$(fabns main)=10.89.0.0/24"
refuse "an IPv6 address" "$W" init "$ROOTFS" --shell --attach "$(fabns main)=fd00::2/64"
refuse "a value without '='" "$W" init "$ROOTFS" --shell --attach "$(fabns main)"
refuse "a file that is not a namespace" "$W" init "$ROOTFS" --shell --attach "/etc/hosts=$IP_WEB"
refuse "a fabric without br0" "$W" init "$ROOTFS" --shell --attach "$(fabns nobridge)=$IP_WEB"
HOSTNET="$(ip -4 -o addr show scope global | awk '{print $4; exit}')"
if [ -n "$HOSTNET" ]; then
  refuse "a subnet overlapping the host's ($HOSTNET)" "$W" init "$ROOTFS" --shell --attach "$(fabns main)=$HOSTNET"
fi
check "refused inits registered no session" [ -z "$(ls "$WORK/i" 2>/dev/null)" ]

# ---------------------------------------------------------------------------
say "2. init: both mains attached to fabric main"
# ---------------------------------------------------------------------------
OUT="$("$W" init "$ROOTFS" --shell --quiet --attach "$(fabns main)=$IP_WEB")" || die "init web: $OUT"
WEB="${OUT%%,*}"
OUT="$("$W" init "$ROOTFS" --shell --quiet --attach "$(fabns main)=$IP_DB")" || die "init db: $OUT"
DB="${OUT%%,*}"
echo "web=$WEB db=$DB"
check "fabric main has both mains' ports" wait_ports main 2
WP0="$(nsenter --net="/run/waypoint/$WEB/main/netns" ip -4 -o addr show wp0 | awk '{print $4}')"
check "web main has $IP_WEB on wp0" [ "$WP0" = "$IP_WEB" ]
check "list shows the attachment" grep -q "addr=$IP_WEB" <("$W" list "$WEB")
check "list --json records the fabric peer" grep -q '"fabric_peer": "wp' <("$W" list "$WEB" --json)
check "workload connected over fabric main" start_workload main
check_replica main

# ---------------------------------------------------------------------------
say "3. snapshot both mains to G behind the barrier"
# ---------------------------------------------------------------------------
check "snapshot web+db main -> G" snapshot_pair main G
check "fabric main keeps both ports across the snapshot" wait_ports main 2
if lat="$(wait_flowing main)"; then ok "main flows again ${lat} ms after the barrier"; else bad "main stalled"; fi
check_replica main

# ---------------------------------------------------------------------------
say "4. fork G into $K replica fabrics"
# ---------------------------------------------------------------------------
refuse "fork --n 2 with --attach" "$W" fork "$WEB" G --n 2 --attach "$(fabns main)=$IP_WEB"
make_fabric bad1 && ip -n "$(fab bad1)" link del br0
refuse "a fork onto a fabric without br0" "$W" fork "$WEB" G --id bad1 --attach "$(fabns bad1)=$IP_WEB"
check "the refused fork left no network pin" [ ! -e "/run/waypoint/$WEB/bad1" ]
t0="$(ms)"
for r in $(seq 1 "$K"); do make_fabric "r$r" && barrier_on "r$r" || die "fabric r$r"; done
pids=()
for r in $(seq 1 "$K"); do fork_pair G "r$r" & pids+=($!); done
rc=0; for p in "${pids[@]}"; do wait "$p" || rc=1; done
check "$((2*K)) attached restores ($(( $(ms) - t0 )) ms)" [ $rc = 0 ]
for r in $(seq 1 "$K"); do x "$DB" "r$r" "echo db-r$r > /id" >/dev/null; barrier_off "r$r"; done
for r in $(seq 1 "$K"); do
  if lat="$(wait_flowing "r$r")"; then ok "r$r flows ${lat} ms after the barrier"; else bad "r$r stalled"; fi
done
for r in $(seq 1 "$K"); do check_replica "r$r"; done
check_replica main

# ---------------------------------------------------------------------------
say "5. second generation: r1 -> G2 -> s1"
# ---------------------------------------------------------------------------
check "snapshot web+db r1 -> G2" snapshot_pair r1 G2
make_fabric s1 && barrier_on s1
check "fork G2 onto fabric s1" fork_pair G2 s1
x "$DB" s1 "echo db-s1 > /id" >/dev/null
barrier_off s1
for r in r1 s1; do
  if lat="$(wait_flowing "$r")"; then ok "$r flows ${lat} ms after the barrier"; else bad "$r stalled"; fi
  check_replica "$r"
done

# ---------------------------------------------------------------------------
say "6. destroy and park remove a fork's end of the fabric"
# ---------------------------------------------------------------------------
"$W" destroy "$WEB" r2 >/dev/null
check "destroy removes web r2's port" wait_ports r2 1
check "parked G3 from r3" snapshot_pair r3 G3 --park
check "park removes both r3 ports" wait_ports r3 0
barrier_on r3
check "re-fork r3 from G3 under the same IDs" fork_pair G3 r3
barrier_off r3
check "fabric r3 has both ports again" wait_ports r3 2
if lat="$(wait_flowing r3)"; then ok "r3 flows ${lat} ms after its re-fork"; else bad "r3 stalled"; fi
check_replica r3

# ---------------------------------------------------------------------------
say "7. suspend ends every attachment; a fork after suspend reattaches"
# ---------------------------------------------------------------------------
"$W" suspend "$WEB" >/dev/null && "$W" suspend "$DB" >/dev/null
# all_empty: wait up to 3s for every replica fabric to lose its ports
all_empty() {
  local i f busy
  for i in $(seq 30); do
    busy=0; for f in main $(seq -f r%g 1 "$K") s1; do [ "$(fabric_ports "$f")" = 0 ] || busy=1; done
    [ $busy = 0 ] && return 0; sleep 0.1
  done
  for f in main $(seq -f r%g 1 "$K") s1; do echo "   $f: $(fabric_ports "$f") ports"; done
  return 1
}
check "no fabric has a port after suspend" all_empty
make_fabric w1 && barrier_on w1
check "fork G onto fabric w1 after suspend" fork_pair G w1
x "$DB" w1 "echo db-w1 > /id" >/dev/null
barrier_off w1
if lat="$(wait_flowing w1)"; then ok "w1 flows ${lat} ms after the barrier"; else bad "w1 stalled"; fi
check_replica w1

# ---------------------------------------------------------------------------
say "8. cleanup leaves nothing behind"
# ---------------------------------------------------------------------------
"$W" cleanup "$WEB" --force >/dev/null && "$W" cleanup "$DB" --force >/dev/null
sleep 0.5
check "fabric w1 has no ports after cleanup" wait_ports w1 0
for f in "${FABRICS[@]}"; do ip netns del "$f" 2>/dev/null; done
FABRICS=()
LEFT="$(leftovers)"
check "no fabrics, pasta, mounts, processes or pins remain ($LEFT)" [ "$LEFT" = "fabrics=0 pasta=0 mounts=0 procs=0 pins=0" ]
WEB="" DB=""
COMPLETED=1
