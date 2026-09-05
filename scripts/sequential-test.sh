#!/usr/bin/env bash
# Basic end-to-end smoke test for the Waypoint v0.6.3 CLI.
#
# This intentionally favors the common Dockerfile-based workflow:
#   build -> exec -> create -> mutate -> restore -> inspect -> cleanup
#
# It creates an offline FROM scratch image containing Bash and a few basic
# commands, so it does not depend on a registry or modify an external build
# context. Run it as root because OverlayFS and CRIU require root privileges.
#
# Usage:
#   sudo ./scripts/sequential-test.sh
#
# Optional:
#   WAYPOINT_BIN=/usr/local/bin/waypoint \
#   WAYPOINT_BASH_INIT_SRC=/usr/local/libexec/waypoint/bash_init \
#     sudo -E ./scripts/sequential-test.sh
#
# Set WAYPOINT_DEMO_FORCE_CLEANUP=1 to exercise `cleanup --force` instead of
# regular cleanup. The default basic smoke path uses regular cleanup.

set -Eeuo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WAYPOINT_BIN="${WAYPOINT_BIN:-$REPO/bin/waypoint}"
BASH_INIT_SRC="${WAYPOINT_BASH_INIT_SRC:-$REPO/bin/bash_init}"

WORK=""
SESSIONS_DIR=""
CONTEXT=""
IMAGE_REF=""
SESSION=""
SHELL_PID=""
BASH_PID=""
JOB_PID=""
PASS=0
CURRENT_SECTION="startup"

say() { CURRENT_SECTION="$*"; printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }
ok() { PASS=$((PASS + 1)); printf '   \033[0;32mok\033[0m  %s\n' "$*"; }
die() { printf '   \033[0;31mFAIL\033[0m %s\n' "$*" >&2; exit 1; }

assert_contains() {
    local description="$1"
    local expected="$2"
    local actual="$3"
    [[ "$actual" == *"$expected"* ]] || die "$description: expected '$expected', got: $actual"
    ok "$description"
}

pid_is_live() {
    local pid="$1"
    [[ -n "$pid" && -d "/proc/$pid" ]]
}

remove_demo_image() {
    [[ -n "$IMAGE_REF" ]] || return 0
    buildah rmi "$IMAGE_REF" >/dev/null 2>&1 || true
    IMAGE_REF=""
}

remove_demo_dirs() {
    if [[ "$WORK" == /tmp/waypoint-v063-demo.* ]]; then
        rm -rf -- "$WORK"
    fi
    if [[ "$SESSIONS_DIR" == /tmp/wp63demo.* ]]; then
        rm -rf -- "$SESSIONS_DIR"
    fi
}

cleanup_on_exit() {
    local status=$?
    trap - EXIT

    if [[ "$status" -ne 0 ]]; then
        printf '\n\033[0;31m!! aborted during "%s"\033[0m\n' "$CURRENT_SECTION" >&2
    fi

    if [[ -n "$SESSION" && -x "$WAYPOINT_BIN" ]]; then
        "$WAYPOINT_BIN" cleanup "$SESSION" >/dev/null 2>&1 || \
            "$WAYPOINT_BIN" cleanup "$SESSION" --force >/dev/null 2>&1 || true
    fi

    # A failed cleanup must not leave this demo's known process tree behind.
    # These PIDs were captured from the just-created session in this run.
    local pid
    for pid in "$JOB_PID" "$BASH_PID" "$SHELL_PID"; do
        if pid_is_live "$pid"; then
            kill -TERM "$pid" >/dev/null 2>&1 || true
        fi
    done

    remove_demo_image
    remove_demo_dirs
    exit "$status"
}
trap cleanup_on_exit EXIT

[[ "${EUID:-$(id -u)}" -eq 0 ]] || die "run this script as root: sudo ./scripts/sequential-test.sh"
[[ -x "$WAYPOINT_BIN" ]] || die "missing $WAYPOINT_BIN; run ./setup build first or set WAYPOINT_BIN"
[[ -x "$BASH_INIT_SRC" ]] || die "missing $BASH_INIT_SRC; run ./setup build first or set WAYPOINT_BASH_INIT_SRC"

for command in buildah criu ldd cp findmnt ps; do
    command -v "$command" >/dev/null 2>&1 || die "missing required command: $command"
done

say "Environment checks"
VERSION_OUT="$("$WAYPOINT_BIN" version)"
assert_contains "candidate version" "v0.6.3" "$VERSION_OUT"
criu check >/dev/null 2>&1 || die "criu check failed"
ok "criu check"

WORK="$(mktemp -d /tmp/waypoint-v063-demo.XXXXXX)"
SESSIONS_DIR="$(mktemp -d /tmp/wp63demo.XXXXXX)"
CONTEXT="$WORK/wpdemocontext$$"
ROOTFS="$CONTEXT/rootfs"
mkdir -p "$ROOTFS"/{bin,tmp,proc,sys,dev,root/demo}

say "Assemble an offline Dockerfile context"
for command in bash cat ls sleep mkdir rm touch; do
    source_path="$(command -v "$command")"
    cp "$source_path" "$ROOTFS/bin/$(basename "$source_path")"
done

for binary in "$ROOTFS"/bin/*; do
    while read -r library; do
        [[ -f "$library" ]] || continue
        mkdir -p "$ROOTFS$(dirname "$library")"
        cp -n "$(readlink -f "$library")" "$ROOTFS$library" 2>/dev/null || true
    done < <(ldd "$binary" 2>/dev/null | grep -oE '/[^ ]+\.so[^ ]*' || true)
done

for loader in /lib/ld-linux-aarch64.so.1 /lib64/ld-linux-x86-64.so.2; do
    if [[ -f "$loader" ]]; then
        mkdir -p "$ROOTFS$(dirname "$loader")"
        cp -n "$loader" "$ROOTFS$loader" 2>/dev/null || true
    fi
done

cat >"$CONTEXT/Dockerfile" <<'DOCKERFILE'
FROM scratch
COPY rootfs /
ENV WAYPOINT_DEMO_IMAGE=ready
WORKDIR /root/demo
DOCKERFILE
ok "temporary Dockerfile context"

export WAYPOINT_SESSIONS_DIR="$SESSIONS_DIR"
export WAYPOINT_BASH_INIT_SRC="$BASH_INIT_SRC"
export WAYPOINT_PRESERVE_SESSION_ON_CLEANUP=false

say "Build a managed shell from the Dockerfile"
BUILD_OUT="$("$WAYPOINT_BIN" build "$CONTEXT" --quiet)"
printf '%s\n' "$BUILD_OUT"
BUILD_CSV="$(printf '%s\n' "$BUILD_OUT" | tail -n 1)"
IFS=, read -r SESSION WORK_OVERLAY SHELL_PID <<<"$BUILD_CSV"
[[ -n "$SESSION" && -n "$WORK_OVERLAY" && "$SHELL_PID" =~ ^[0-9]+$ ]] || \
    die "could not parse quiet build output: $BUILD_CSV"
[[ -d "$SESSIONS_DIR/$SESSION" ]] || die "session directory was not created"
pid_is_live "$SHELL_PID" || die "reported shell PID $SHELL_PID is not live"
BASH_PID="$(ps -o pid= --ppid "$SHELL_PID" | awk 'NR == 1 {print $1}')"
ok "Dockerfile build and managed shell startup"

context_name="$(basename "$CONTEXT")"
IMAGE_REF="$(buildah images --format '{{.Name}}:{{.Tag}}' | \
    grep -F "localhost/waypoint_${context_name}:" | head -n 1 || true)"

say "Verify image configuration and persistent shell state"
STATE_OUT="$("$WAYPOINT_BIN" exec "$SESSION" \
    'GREETING=hello-v063; sleep 300 & echo job=$!; echo base-content > base.txt; echo image=$WAYPOINT_DEMO_IMAGE cwd=$(pwd) GREETING=$GREETING')"
printf '%s\n' "$STATE_OUT"
assert_contains "image ENV applied" "image=ready" "$STATE_OUT"
assert_contains "image WORKDIR applied" "cwd=/root/demo" "$STATE_OUT"
assert_contains "shell variable set" "GREETING=hello-v063" "$STATE_OUT"
JOB_PID="$(printf '%s\n' "$STATE_OUT" | sed -n 's/.*job=\([0-9][0-9]*\).*/\1/p' | tail -n 1)"
pid_is_live "$JOB_PID" || die "background job PID was not preserved"

PERSIST_OUT="$("$WAYPOINT_BIN" exec "$SESSION" \
    'echo GREETING=$GREETING cwd=$(pwd); cat base.txt; jobs -r')"
printf '%s\n' "$PERSIST_OUT"
assert_contains "shell state persists across exec" "GREETING=hello-v063" "$PERSIST_OUT"
assert_contains "filesystem state persists across exec" "base-content" "$PERSIST_OUT"
assert_contains "background job persists across exec" "sleep 300" "$PERSIST_OUT"

say "Checkpoint A, diverge, and restore"
CREATE_OUT="$("$WAYPOINT_BIN" create "$SESSION" A)"
printf '%s\n' "$CREATE_OUT"
assert_contains "checkpoint A created" "created successfully" "$CREATE_OUT"

"$WAYPOINT_BIN" exec "$SESSION" \
    'GREETING=mutated; cd /tmp; echo changed > /root/demo/base.txt; echo transient > /root/transient.txt' >/dev/null

RESTORE_OUT="$("$WAYPOINT_BIN" restore "$SESSION" A)"
printf '%s\n' "$RESTORE_OUT"
assert_contains "checkpoint A restored" "restored, new PID" "$RESTORE_OUT"

ROLLBACK_OUT="$("$WAYPOINT_BIN" exec "$SESSION" \
    'echo GREETING=$GREETING cwd=$(pwd); cat base.txt; if [ -e /root/transient.txt ]; then echo rollback-broken; else echo transient-absent; fi; jobs -r')"
printf '%s\n' "$ROLLBACK_OUT"
assert_contains "memory state rolled back" "GREETING=hello-v063" "$ROLLBACK_OUT"
assert_contains "cwd rolled back" "cwd=/root/demo" "$ROLLBACK_OUT"
assert_contains "filesystem content rolled back" "base-content" "$ROLLBACK_OUT"
assert_contains "later file removed by rollback" "transient-absent" "$ROLLBACK_OUT"
assert_contains "background job restored" "sleep 300" "$ROLLBACK_OUT"

say "Create checkpoint B and traverse the chain"
"$WAYPOINT_BIN" exec "$SESSION" \
    'GREETING=state-B; echo checkpoint-B-content > B.txt' >/dev/null
"$WAYPOINT_BIN" create "$SESSION" B >/dev/null

"$WAYPOINT_BIN" restore "$SESSION" A >/dev/null
A_OUT="$("$WAYPOINT_BIN" exec "$SESSION" \
    'echo GREETING=$GREETING; if [ -e B.txt ]; then echo B-visible; else echo B-absent; fi')"
assert_contains "older checkpoint restores old memory" "GREETING=hello-v063" "$A_OUT"
assert_contains "older checkpoint hides B layer" "B-absent" "$A_OUT"

"$WAYPOINT_BIN" restore "$SESSION" B >/dev/null
B_OUT="$("$WAYPOINT_BIN" exec "$SESSION" \
    'echo GREETING=$GREETING cwd=$(pwd); cat base.txt B.txt')"
printf '%s\n' "$B_OUT"
assert_contains "newer checkpoint restores new memory" "GREETING=state-B" "$B_OUT"
assert_contains "checkpoint chain includes A" "base-content" "$B_OUT"
assert_contains "checkpoint chain includes B" "checkpoint-B-content" "$B_OUT"

say "Inspect the session and checkpoints"
LIST_OUT="$("$WAYPOINT_BIN" list "$SESSION")"
printf '%s\n' "$LIST_OUT"
assert_contains "list includes A" "  A" "$LIST_OUT"
assert_contains "list includes B" "  B" "$LIST_OUT"

INFO_OUT="$("$WAYPOINT_BIN" info "$SESSION" B)"
assert_contains "info identifies checkpoint B" '"checkpoint_id": "B"' "$INFO_OUT"
assert_contains "info identifies the session" "$SESSION" "$INFO_OUT"

say "Clean up and check for leaks"
if [[ "${WAYPOINT_DEMO_FORCE_CLEANUP:-0}" == "1" ]]; then
    "$WAYPOINT_BIN" cleanup "$SESSION" --force
else
    "$WAYPOINT_BIN" cleanup "$SESSION"
fi

if findmnt -rn | grep -F "$SESSION" >/dev/null; then
    die "a mount still references session $SESSION"
fi
[[ ! -e "$SESSIONS_DIR/$SESSION" ]] || die "session directory still exists"
[[ ! -e "/tmp/waypoint-sessions-info/$SESSION.json" ]] || die "session registry entry still exists"
pid_is_live "$SHELL_PID" && die "bash_init PID $SHELL_PID survived cleanup"
pid_is_live "$BASH_PID" && die "guest Bash PID $BASH_PID survived cleanup"
pid_is_live "$JOB_PID" && die "background job PID $JOB_PID survived cleanup"
ok "cleanup removed mounts, files, registry, and processes"

SESSION=""
remove_demo_image
say "Summary"
printf '   \033[1;32m%d checks passed\033[0m\n' "$PASS"
