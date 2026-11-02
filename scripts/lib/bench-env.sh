# Shared by benchmark.sh and membench.sh; sourced, not run. Expects REPO (the
# checkout), WORK (the private temp dir), BIN (where to build), CSV and LABEL,
# and WAYPOINT_BIN with BASH_INIT_BIN when the caller is not to build.
#
# Sourcing it points Waypoint at WORK and pins the config knobs that change
# what is measured, so that the host's config file cannot: tmpfs_images,
# incremental_dumps and phase_stats off and log level 2, the settings of the
# baseline numbers, unless the environment sets them. The functions, in the
# order the scripts call them:
#   bench_require           root, criu and a static busybox
#   bench_csv_header <h>    writes <h> to an empty CSV; refuses a CSV whose
#                           header differs
#   bench_binaries          sets W and WAYPOINT_BASH_INIT_SRC: the given
#                           binaries, or this checkout built into BIN
#   bench_env               probes the environment into ENV_PREFIX, the
#                           first CSV columns (ENV_HEADER), and prints it
#   wpinfo <key>            a config key's effective value, from `waypoint info`

export WAYPOINT_SESSIONS_DIR="$WORK/s" WAYPOINT_SESSION_INFO_DIR="$WORK/i"
export WAYPOINT_PRESERVE_SESSION_ON_CLEANUP=false
export WAYPOINT_TMPFS_IMAGES="${WAYPOINT_TMPFS_IMAGES:-false}"
export WAYPOINT_INCREMENTAL_DUMPS="${WAYPOINT_INCREMENTAL_DUMPS:-false}"
export WAYPOINT_PHASE_STATS="${WAYPOINT_PHASE_STATS:-false}"
export WAYPOINT_LOG_LEVEL="${WAYPOINT_LOG_LEVEL:-2}"

# utc is the same for all rows of one run; images_fs is the file system the
# images are written to; criu_gitid is the second line of `criu --version`;
# waypoint_commit is the commit the binary was built from, -dirty with
# uncommitted changes; log_level is the effective value.
# shellcheck disable=SC2034  # read by the scripts
ENV_HEADER="utc,label,host,kernel,cpu,nproc,governor,images_fs,criu,criu_gitid,waypoint,waypoint_commit,log_level"

bench_require() {
  local c
  [ "$(id -u)" -eq 0 ] || { echo "must run as root"; exit 1; }
  for c in criu busybox; do
    command -v "$c" >/dev/null || { echo "missing required command: $c"; exit 1; }
  done
  if ldd "$(command -v busybox)" >/dev/null 2>&1; then
    echo "busybox must be statically linked (package busybox-static)"; exit 1
  fi
}

bench_csv_header() {
  [ -n "$CSV" ] || return 0
  if [ ! -s "$CSV" ]; then
    echo "$1" >"$CSV"
  elif [ "$(head -n 1 "$CSV")" != "$1" ]; then
    echo "$CSV has a different header; use a new file" >&2; exit 1
  fi
}

bench_binaries() {
  local cand
  GO="$(command -v go || true)"
  for cand in /usr/local/go/bin/go "$HOME/go/bin/go" "/home/${SUDO_USER:-}/go/bin/go"; do
    [ -n "$GO" ] && break
    [ -x "$cand" ] && GO="$cand"
  done
  if [ -n "${WAYPOINT_BIN:-}" ] && [ -n "${BASH_INIT_BIN:-}" ]; then
    W="$WAYPOINT_BIN"; export WAYPOINT_BASH_INIT_SRC="$BASH_INIT_BIN"
  elif [ -n "${WAYPOINT_BIN:-}${BASH_INIT_BIN:-}" ]; then
    echo "set both WAYPOINT_BIN and BASH_INIT_BIN, or neither (then this checkout is built)"; exit 1
  else
    [ -n "$GO" ] || { echo "go toolchain not found; set WAYPOINT_BIN and BASH_INIT_BIN"; exit 1; }
    mkdir -p "$BIN"
    ( cd "$REPO" || exit 1
      env GOCACHE="${GOCACHE:-/tmp/waypoint-go-cache}" "$GO" build -o "$BIN/waypoint" ./cmd/waypoint
      env GOCACHE="${GOCACHE:-/tmp/waypoint-go-cache}" CGO_ENABLED=0 "$GO" build -o "$BIN/bash_init" ./cmd/bash-init )
    W="$BIN/waypoint"; export WAYPOINT_BASH_INIT_SRC="$BIN/bash_init"
  fi
}

# wpinfo <key>: the effective value of a config key as `waypoint info`
# reports it, booleans as 0/1; "-" without python3 or `waypoint info`.
wpinfo() {
  "$W" info 2>/dev/null | python3 -I -c '
import json, sys
v = json.load(sys.stdin)["config"][sys.argv[1]]["value"]
print(int(v) if isinstance(v, bool) else v)' "$1" 2>/dev/null || echo -
}

# wpcommit: the commit the waypoint binary was built from, read from the
# build info Go records; "-dirty" marks uncommitted changes.
wpcommit() {
  { [ -n "$GO" ] && "$GO" version -m "$W" 2>/dev/null || true; } | awk '
    $1 == "build" && index($2, "vcs.revision=") == 1 { r = substr($2, 14, 12) }
    $1 == "build" && $2 == "vcs.modified=true" { m = "-dirty" }
    END { print (r == "" ? "-" : r m) }'
}

bench_env() {
  CPU="$(awk -F': ' '/^model name/ { print $2; exit }' /proc/cpuinfo)"
  [ -n "$CPU" ] || CPU="$(lscpu 2>/dev/null | awk -F': +' '/^Model name/ { print $2; exit }')"
  GOVERNOR="$(cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_governor 2>/dev/null || echo unknown)"
  HOST="$(hostname -f 2>/dev/null || hostname -s)"
  CRIU_VERSION="$(criu --version | awk '/^Version/ { print $2; exit }')"
  CRIU_GITID="$(criu --version | awk '/^GitID/ { print $2; exit }')"
  WP_VERSION="$("$W" version | awk '{ print $NF }')"
  WP_COMMIT="$(wpcommit)"
  LOG_LEVEL="$(wpinfo waypoint_log_level)"
  IMAGES_FS="$(findmnt -no FSTYPE -T "$WORK" 2>/dev/null || stat -f -c %T "$WORK")"
  IMAGES_DIR="$WORK"
  [ "$(wpinfo tmpfs_images)" != 1 ] || { IMAGES_FS=tmpfs; IMAGES_DIR="$(wpinfo tmpfs_images_dir)"; }
  # shellcheck disable=SC2034  # read by the scripts
  ENV_PREFIX="$(date -u +%FT%TZ),$(printf %s "$LABEL" | tr ',\n' '  '),$HOST,$(uname -r),$(printf %s "${CPU:-unknown}" | tr ',' ' '),$(nproc),$GOVERNOR,$IMAGES_FS,$CRIU_VERSION,${CRIU_GITID:--},$WP_VERSION,$WP_COMMIT,$LOG_LEVEL"
  printf 'waypoint %s %s (%s) on %s: Linux %s, %s x %s (%s), criu %s %s, images on %s (%s), log level %s\n' \
    "$WP_VERSION" "$WP_COMMIT" "$W" "$HOST" "$(uname -r)" "$(nproc)" "${CPU:-unknown cpu}" "$GOVERNOR" \
    "$CRIU_VERSION" "${CRIU_GITID:--}" "$IMAGES_DIR" "$IMAGES_FS" "$LOG_LEVEL"
}
