#!/bin/bash
# Runtime test of `waypoint compose`: a two-service project whose services
# reach each other by name, and whose replicas keep their TCP connections
# across group checkpoints, forks, park, suspend, and a second generation.
#
# The project: db runs the fabric-peer servers, web the clients, connected to
# "db" by name over the project fabric. db is built from a Dockerfile (FROM
# scratch + COPY, so no registry is needed) and starts from the image's CMD;
# without buildah it uses x-waypoint-rootfs instead. Clients never reconnect,
# so a broken connection stays visible; each replica's db answers with its
# own /id, so a replica can only be served by its own copy.
#
# Requires root, criu, pasta (package passt), and Go unless WAYPOINT_BIN,
# BASH_INIT_BIN and FABRIC_PEER_BIN are given; buildah optionally. K sets the
# replica count (default 3, at least 3). If SELinux confines pasta (see
# `./setup check`), an unconfined copy is used. Sessions, registry, and rootfs
# are private to the test.
#
# Usage:  sudo ./scripts/test-compose.sh
set -uo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d /tmp/wpcmp.XXXXXX)"
ROOTFS="$WORK/project/rootfs"
BIN="$WORK/bin"
K="${K:-3}"
P="wpt$(basename "$WORK" | cut -d. -f2 | tr 'A-Z' 'a-z')"
CURRENT_SECTION="startup"
COMPLETED=0
PASS=0; FAIL=0; SKIP=0

export WAYPOINT_SESSIONS_DIR="$WORK/s" WAYPOINT_SESSION_INFO_DIR="$WORK/i"
export WAYPOINT_PRESERVE_SESSION_ON_CLEANUP=false
unset WAYPOINT_NETWORK # compose sessions are always private

say()  { CURRENT_SECTION="$*"; printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
ok()   { PASS=$((PASS+1)); printf '   \033[0;32mok\033[0m  %s\n' "$*"; }
bad()  { FAIL=$((FAIL+1)); printf '   \033[0;31mFAIL\033[0m %s\n' "$*"; }
skip() { SKIP=$((SKIP+1)); printf '   \033[0;33mskip\033[0m %s\n' "$*"; }
check() { local desc="$1"; shift; if "$@"; then ok "$desc"; else bad "$desc"; fi; }
refuse() { local desc="$1"; shift; if "$@" >/dev/null 2>&1; then bad "$desc was accepted"; else ok "$desc is refused"; fi; }
ms()   { date +%s%3N; }
die()  { bad "$*"; exit 1; }

cleanup() {
  if [ "$COMPLETED" -ne 1 ]; then
    printf '\n\033[0;31m!! aborted during "%s"\033[0m\n' "$CURRENT_SECTION"
    "$W" compose cleanup "$P" --force >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK"
  printf '\n%d passed, %d failed, %d skipped\n' "$PASS" "$FAIL" "$SKIP"
  [ "$FAIL" -eq 0 ] && [ "$COMPLETED" -eq 1 ]
}
trap cleanup EXIT

wc_() { "$W" compose "$@"; }
x()     { wc_ exec "$P" "$1" "$2" -- "$3"; }
field() { tr ' ' '\n' <<<"$1" | sed -n "s/^$2=//p"; }
cli()   { x "$1" web 'cat /tmp/cli.status'; }
srv()   { x "$1" db 'cat /tmp/srv.status'; }
bulk()  { x "$1" db 'cat /tmp/bulk.status'; }
replicas() { wc_ list "$P" --json | python3 -c 'import json,sys; print(" ".join(r["id"] for r in json.load(sys.stdin)["replicas"]))'; }
pins() { ls /run/waypoint/groups/"$P" 2>/dev/null | tr '\n' ' ' | sed 's/ $//'; }
# wait_flowing <replica>: wait up to 60s for both streams to advance
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
flowing() { local lat; if lat="$(wait_flowing "$1")"; then ok "$1 flows (${lat} ms)"; else bad "$1 stalled"; fi; }
# check_replica <replica>: connections alive, intact, never reconnected, and
# answered by this replica's own db
check_replica() {
  local c s b
  c="$(cli "$1")"; s="$(srv "$1")"; b="$(bulk "$1")"
  [ "$(field "$c" state)" = ok ] && [ "$(field "$c" errors)" = 0 ] &&
    ok "$1: web's connection to db alive" || bad "$1: client $c"
  [ "$(field "$c" peer)" = "db-$1" ] && ok "$1: answered by db-$1" || bad "$1: answered by '$(field "$c" peer)'"
  [ "$(field "$s" conns)" = 1 ] && [ "$(field "$s" gaps)" = 0 ] &&
    ok "$1: request stream intact on its first connection" || bad "$1: server $s"
  [ "$(field "$b" conns)" = 1 ] && [ "$(field "$b" gaps)" = 0 ] && [ "$(field "$b" state)" = ok ] &&
    ok "$1: bulk stream intact on its first connection" || bad "$1: bulk $b"
}
set_id() { x "$1" db "echo db-$1 > /id" >/dev/null; }

# ---------------------------------------------------------------------------
say "Environment checks and build"
# ---------------------------------------------------------------------------
[ "$(id -u)" -eq 0 ] || { echo "must run as root"; exit 1; }
[ "$K" -ge 3 ] || { echo "K must be at least 3"; exit 1; }
for c in criu pasta python3; do
  command -v "$c" >/dev/null || { echo "missing required command: $c"; exit 1; }
done
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

SERVERS='fabric-peer server db:9000 >/tmp/srv.log 2>&1 & fabric-peer bulk-server db:9001 >/tmp/bulk.log 2>&1 & wait'
if command -v buildah >/dev/null; then
  BUILT=1
  cat >"$WORK/project/Dockerfile.db" <<EOF
FROM scratch AS base
COPY rootfs/ /
FROM base AS db
ARG ROLE=unset
ENV ROLE=\$ROLE GODEBUG=multipathtcp=0
CMD ["/bin/bash", "-c", "$SERVERS"]
EOF
  DB_SOURCE='build:
      context: .
      dockerfile: Dockerfile.db
      target: db
      args:
        ROLE: database'
else
  BUILT=0
  DB_SOURCE="x-waypoint-rootfs: ./rootfs
    environment:
      ROLE: database
      GODEBUG: multipathtcp=0
    command: [\"/bin/bash\", \"-c\", \"$SERVERS\"]"
fi
cat >"$WORK/project/compose.yaml" <<EOF
services:
  web:
    x-waypoint-rootfs: ./rootfs
    depends_on: [db]
    working_dir: /tmp
    environment:
      GODEBUG: multipathtcp=0
    command: ["/bin/bash", "-c", "fabric-peer client db:9000 >/tmp/cli.log 2>&1 & fabric-peer bulk-client db:9001 >/tmp/bulkc.log 2>&1 & wait"]
    ports: ["8080:80"]
  db:
    $DB_SOURCE
EOF

# ---------------------------------------------------------------------------
say "1. up: two services on one fabric, reachable by name"
# ---------------------------------------------------------------------------
UP_ERR="$WORK/up.err"
( cd "$WORK/project" && "$W" compose up -p "$P" ) >"$WORK/up.out" 2>"$UP_ERR" || { cat "$WORK/up.out" "$UP_ERR"; die "compose up failed"; }
cat "$WORK/up.out"
check "unsupported settings are warned about (ports)" grep -q "web: ports is ignored" "$UP_ERR"
check "list shows the project" grep -qx "$P" <(wc_ list)
check "main replica is running" [ "$(replicas)" = main ]
check "fabric main is pinned" [ "$(pins)" = main ]
check "db's hostname is its service name" [ "$(x main db 'cat /proc/sys/kernel/hostname')" = db ]
check "web resolves db through /etc/hosts" grep -q ' db$' <(x main web 'cat /etc/hosts')
check "web starts in its working_dir" [ "$(x main web 'pwd')" = /tmp ]
check "db has its environment" [ "$(x main db 'echo $ROLE')" = database ]
if [ "$BUILT" = 1 ]; then
  ok "db was built from Dockerfile.db (target db, build arg) and started from the image CMD"
else
  skip "buildah not installed: db uses x-waypoint-rootfs instead of a build"
fi
set_id main
for i in $(seq 100); do [ "$(field "$(cli main)" peer)" = db-main ] && break; sleep 0.1; done
check_replica main

# ---------------------------------------------------------------------------
say "2. group checkpoint G of main, fork G into $K replicas"
# ---------------------------------------------------------------------------
t0="$(ms)"
check "checkpoint main -> G" wc_ checkpoint "$P" G
echo "   (checkpoint took $(( $(ms) - t0 )) ms)"
refuse "a second checkpoint G" wc_ checkpoint "$P" G
flowing main
check_replica main
t0="$(ms)"
IDS="$(wc_ fork "$P" G --n "$K")" || die "fork failed: $IDS"
echo "   (fork of $K replicas took $(( $(ms) - t0 )) ms)"
check "fork --n $K printed $K replica IDs" [ "$(wc -w <<<"$IDS")" = "$K" ]
for r in $IDS; do set_id "$r"; done
for r in $IDS; do flowing "$r"; done
for r in $IDS; do check_replica "$r"; done
check_replica main
check "every replica has a fabric" [ "$(wc -w <<<"$(pins)")" = $((K + 1)) ]
set -- $IDS; R1="$1" R2="$2" R3="$3"

# ---------------------------------------------------------------------------
say "3. second generation: $R1 -> G2 -> s1"
# ---------------------------------------------------------------------------
check "snapshot $R1 -> G2" wc_ snapshot "$P" "$R1" G2
check "fork G2 as s1" wc_ fork "$P" G2 --id s1
set_id s1
flowing s1; flowing "$R1"
check_replica s1
check_replica "$R1"
check "G2's parent is G" grep -q '"parent": "G"' <(wc_ list "$P" --json)

# ---------------------------------------------------------------------------
say "4. park, destroy, and refusals"
# ---------------------------------------------------------------------------
check "park $R2 as G3" wc_ snapshot "$P" "$R2" G3 --park
check "parked replica is gone with its fabric" bash -c "! grep -qw $R2 <<<'$(replicas) $(pins)'"
check "fork G3 back under the same ID" wc_ fork "$P" G3 --id "$R2"
flowing "$R2"
check_replica "$R2"
check "destroy $R3" wc_ destroy "$P" "$R3"
check "destroyed replica is gone with its fabric" bash -c "! grep -qw $R3 <<<'$(replicas) $(pins)'"
refuse "destroy main" wc_ destroy "$P" main
refuse "park main" wc_ snapshot "$P" main G4 --park
refuse "a second up of the project" bash -c "cd '$WORK/project' && '$W' compose up -p '$P'"
refuse "fork of an unknown checkpoint" wc_ fork "$P" nope
refuse "exec in an unknown service" wc_ exec "$P" main cache -- true
refuse "a replica ID that exists" wc_ fork "$P" G --id s1
check "the refused fork left s1 running" [ "$(field "$(cli s1)" state)" = ok ]
WEB="$(wc_ list "$P" --json | python3 -c 'import json,sys; print([s["session_id"] for s in json.load(sys.stdin)["project"]["services"] if s["name"]=="web"][0])')"
refuse "a plain fork of a member session" "$W" fork "$WEB" G --id solo
refuse "a plain snapshot of a member's replica" "$W" snapshot "$WEB" s1 G5
refuse "a plain checkpoint of a member session" "$W" checkpoint "$WEB" G5
refuse "a plain destroy of a member's replica" "$W" destroy "$WEB" s1
refuse "a plain suspend of a member session" "$W" suspend "$WEB"
refuse "a plain cleanup of a member session" "$W" cleanup "$WEB" --force
check "the refusal names the project" grep -q "compose project $P" <("$W" fork "$WEB" G 2>&1)
check "attaching a fork to a project fabric is refused" grep -q "compose project's fabric" <("$W" fork any G --attach "/run/waypoint/groups/$P/main/netns=10.89.0.9/24" 2>&1)
check "exec in a member session still works" [ "$("$W" exec "$WEB" s1 -- 'echo hi')" = hi ]
check "s1 is untouched by the refusals" [ "$(field "$(cli s1)" state)" = ok ]

# ---------------------------------------------------------------------------
say "5. suspend ends every replica; forking resumes"
# ---------------------------------------------------------------------------
check "suspend" wc_ suspend "$P"
check "no replicas after suspend" [ -z "$(replicas)" ]
check "no fabrics after suspend" [ -z "$(pins)" ]
check "fork G as w1 after suspend" wc_ fork "$P" G --id w1
set_id w1
flowing w1
check_replica w1

# ---------------------------------------------------------------------------
say "6. cleanup leaves nothing behind"
# ---------------------------------------------------------------------------
SESSIONS="$(wc_ list "$P" --json | python3 -c 'import json,sys; print(" ".join(s["session_id"] for s in json.load(sys.stdin)["project"]["services"]))')"
check "cleanup" wc_ cleanup "$P"
check "project is gone" [ "$(wc_ list)" = "No projects found" ]
check "member sessions are gone" [ -z "$(ls "$WORK/i"/*.json 2>/dev/null)" ]
check "no fabric pins remain" [ ! -e "/run/waypoint/groups/$P" ]
for s in $SESSIONS; do
  check "no network pins remain for session $s" [ ! -e "/run/waypoint/$s" ]
done
check "no mounts remain" [ "$(grep -c "$WORK" /proc/self/mountinfo)" = 0 ]
check "no processes remain" [ "$(for p in /proc/[0-9]*; do readlink "$p/root" 2>/dev/null; done | grep -c "$WORK")" = 0 ]
check "no pasta remains" [ -z "$(for s in $SESSIONS; do pgrep -f -- "--netns /run/waypoint/$s/"; done)" ]
COMPLETED=1
