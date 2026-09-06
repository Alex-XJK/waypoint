#!/usr/bin/env bash
# Root-only, offline integration/regression test of agent-style search.
#
# sudo env BRANCHES=8 GENERATIONS=3 ./scripts/test-search-workflow.sh
# Optional: WAYPOINT_BIN, BASH_INIT_BIN (otherwise build into the artifact dir),
# WORKFLOW_ARTIFACT_DIR, WAYPOINT_TMPFS_IMAGES, WAYPOINT_PHASE_STATS.
# BRANCHES >= 4; generations 1..3 progressively repair a Python normalizer;
# later generations exercise deeper checkpoint chains with the solved program.
# No image pulls, network isolation assertions, suspend, or host package changes.
# Every Waypoint invocation runs as root. Only private sessions and build image
# tags named for this run are cleaned. Commands, output, timing, scorecards,
# exported files, and host cleanup evidence remain in WORKFLOW_ARTIFACT_DIR.
set -euo pipefail
[[ $EUID == 0 ]] || { echo 'Run this test with sudo (CRIU/mounts require root).' >&2; exit 1; }
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BRANCHES="${BRANCHES:-8}"
GENERATIONS="${GENERATIONS:-3}"
[[ $BRANCHES =~ ^[0-9]+$ && $GENERATIONS =~ ^[0-9]+$ ]] || { echo 'BRANCHES and GENERATIONS must be integers.' >&2; exit 1; }
(( BRANCHES >= 4 && BRANCHES <= 128 && GENERATIONS >= 1 && GENERATIONS <= 20 )) || {
  echo 'Use BRANCHES=4..128 and GENERATIONS=1..20.' >&2; exit 1; }
WORK="$(mktemp -d /tmp/ws.XXXXXX)"
ART="${WORKFLOW_ARTIFACT_DIR:-$REPO/../waypoint-test-results/search-$(date -u +%Y%m%d-%H%M%S)-$$}"
mkdir -p "$ART"; ART="$(cd "$ART" && pwd)"
mkdir -p "$ART/commands" "$ART/export" "$ART/preparation/project/data" "$ART/bin" "$WORK/s" "$WORK/i"
exec > >(tee "$ART/run.log") 2>&1
export WAYPOINT_SESSIONS_DIR="$WORK/s" WAYPOINT_SESSION_INFO_DIR="$WORK/i"
export WAYPOINT_PRESERVE_SESSION_ON_CLEANUP=false
export WAYPOINT_TMPFS_DIR="/dev/shm/$(basename "$WORK")"
export WAYPOINT_TMPFS_IMAGES="${WAYPOINT_TMPFS_IMAGES:-false}"
export WAYPOINT_PHASE_STATS="${WAYPOINT_PHASE_STATS:-false}"
W="${WAYPOINT_BIN:-$ART/bin/waypoint}"
HELPER="${BASH_INIT_BIN:-$ART/bin/bash_init}"
IMAGE_CONTEXT="$WORK/context-$(basename "$WORK" | tr '[:upper:].' '[:lower:]-')"
IMAGE_PREFIX="localhost/waypoint_$(basename "$IMAGE_CONTEXT"):"
mkdir -p "$IMAGE_CONTEXT/rootfs"
SESSION=''; PASS=0; PHASE=setup; COMPLETED=0; pids=()
STARTED="$(date -u +%FT%TZ)"
printf 'result\tbehavior\n' > "$ART/assertions.tsv"
printf 'generation\tcandidate\tflag_mask\tscore\tstatus\n' > "$ART/scores.tsv"
printf 'start_ns\telapsed_ms\texit_code\tcommand\toutput\n' > "$ART/commands.tsv"

# A separate file per call keeps concurrent command output intact. The TSV is
# one append per command, so it can be sorted by start_ns for chronological use.
wp() {
  local log start end rc=0 quoted
  log="$(mktemp "$ART/commands/call.XXXXXX")"
  printf -v quoted '%q ' "$W" "$@"
  start="$(date +%s%N)"
  "$W" "$@" > "$log" 2>&1 || rc=$?
  end="$(date +%s%N)"
  printf '%s\t%s\t%s\t%s\t%s\n' "$start" "$(((end-start)/1000000))" "$rc" "$quoted" "$log" >> "$ART/commands.tsv"
  cat "$log"
  return "$rc"
}
check() {
  local description=$1; shift
  if "$@"; then
    PASS=$((PASS+1)); printf 'PASS\t%s\n' "$description" | tee -a "$ART/assertions.tsv"
  else
    printf 'FAIL\t%s\n' "$description" | tee -a "$ART/assertions.tsv"
    return 1
  fi
}
expect_failure() {
  local description=$1; shift
  local rc=0
  wp "$@" > "$ART/expected-failure-$(date +%s%N).log" || rc=$?
  check "$description" test "$rc" -ne 0
}
section() { PHASE=$*; printf '\n== %s ==\n' "$PHASE"; }

# Host-side evidence helper. Normal checks are read-only; on an aborted run,
# finish-jobs may stop only the recorded command jobs and their descendants.
cat > "$ART/evidence.py" <<'PY'
import glob, hashlib, json, os, signal, stat, sys, time
from pathlib import Path
mode, *args = sys.argv[1:]
def digest(root):
    out=[]
    for p in sorted(Path(root).rglob('*')):
        s=p.lstat()
        if stat.S_ISLNK(s.st_mode): value='link:'+os.readlink(p)
        elif stat.S_ISREG(s.st_mode): value=hashlib.sha256(p.read_bytes()).hexdigest()
        elif stat.S_ISDIR(s.st_mode): value='dir'
        else: continue
        out.append([str(p.relative_to(root)),stat.S_IMODE(s.st_mode),value])
    return out
if mode=='finish-jobs':
    def proc(pid):
        try:
            fields=Path(f'/proc/{pid}/stat').read_text().rsplit(')',1)[1].split()
            return int(fields[1]),fields[19],fields[0]
        except OSError: return None
    identities={}
    for raw in args:
        if raw.isdigit():
            pid=int(raw); state=proc(pid)
            if state and state[0]==os.getppid() and state[2]!='Z':
                identities[pid]=state[1]
    deadline=time.monotonic()+30
    while identities:
        active={pid for pid,start in identities.items() if (p:=proc(pid)) and p[1]==start and p[2]!='Z'}
        if not active: break
        if time.monotonic()>=deadline:
            # Capture descendants before terminating parents. Never signal a
            # PID whose start time no longer matches the captured process.
            while True:
                added=False
                for entry in glob.glob('/proc/[0-9]*'):
                    pid=int(entry.rsplit('/',1)[1]); state=proc(pid)
                    if state and state[0] in active and pid not in active:
                        active.add(pid); identities[pid]=state[1]; added=True
                if not added: break
            for sig in (signal.SIGTERM,signal.SIGKILL):
                for pid in sorted(active,reverse=True):
                    state=proc(pid)
                    if state and state[1]==identities[pid]:
                        try: os.kill(pid,sig)
                        except ProcessLookupError: pass
                time.sleep(0.25)
            print('Stopped unfinished command jobs after 30-second cleanup grace period.')
            break
        time.sleep(0.1)
elif mode=='hash':
    print(json.dumps(digest(args[0]),sort_keys=True))
elif mode=='capture':
    records=[]
    for p in glob.glob(args[0]+'/*/forks/*/fork.json'):
        f=json.load(open(p)); pid=f.get('pid',0)
        try:
            fields=Path(f'/proc/{pid}/stat').read_text().rsplit(')',1)[1].split()
            records.append({'fork':f['id'],'pid':pid,'start':fields[19]})
        except OSError: pass
    print(json.dumps(records,indent=2))
elif mode=='clean':
    problems=[]
    for f in json.load(open(args[0])):
        try:
            fields=Path(f"/proc/{f['pid']}/stat").read_text().rsplit(')',1)[1].split()
            if fields[19]==f['start'] and fields[0]!='Z': problems.append(f"surviving identity: {f}")
        except OSError: pass
    private=args[1]
    for p in glob.glob('/proc/[0-9]*'):
        try:
            root=os.readlink(p+'/root')
            if root.startswith(private+'/'): problems.append(f'process root: {p} {root}')
            mounts=Path(p+'/mountinfo').read_text()
            if private+'/' in mounts: problems.append(f'mounts: {p}')
        except (OSError,PermissionError): pass
    if problems: print('\n'.join(problems)); sys.exit(1)
    print('No live captured process identities, private process roots, or private mounts.')
elif mode=='dag':
    j=json.load(open(args[0])); parents={c['id']:c.get('parent_id','') for c in j['checkpoints']}
    assert parents['prepared']=='boot',parents
    previous='prepared'
    for g in range(1,int(args[1])+1):
        assert parents[f'winner-{g}']==previous,parents
        previous=f'winner-{g}'
    print(json.dumps(parents,indent=2))
PY
HOST_HOSTNAME="$(cat /proc/sys/kernel/hostname)"
HOST_PTMX="$(stat -Lc '%F %t:%T %a' /dev/ptmx) $(readlink /dev/ptmx || true)"
printf '%s\n' "$HOST_HOSTNAME" > "$ART/host-hostname.before"
printf '%s\n' "$HOST_PTMX" > "$ART/host-ptmx.before"

cleanup() {
  local rc=$? sid record cleanup_failed=0 image pid
  trap - EXIT INT TERM
  set +e
  /usr/bin/python3 "$ART/evidence.py" finish-jobs "${pids[@]}" > "$ART/command-job-cleanup.log" 2>&1
  for pid in "${pids[@]}"; do wait "$pid" 2>/dev/null || true; done
  # The registry is private, so this also finds a session whose build failed
  # after registration and before the quiet session ID could be returned.
  /usr/bin/python3 "$ART/evidence.py" capture "$WORK/s" > "$ART/processes-before-cleanup.json"
  for record in "$WORK/i"/*.json; do
    [[ -f $record ]] || continue
    sid="$(basename "$record" .json)"
    wp cleanup "$sid" --force > "$ART/cleanup-$sid.log" || cleanup_failed=1
  done
  # Match the unique context name, never all Buildah images or preexisting tags.
  while IFS= read -r image; do
    [[ $image == "$IMAGE_PREFIX"* ]] || continue
    printf '%s\n' "$image" >> "$ART/removed-image-tags.txt"
    buildah rmi "$image" >> "$ART/buildah-cleanup.log" 2>&1 || cleanup_failed=1
  done < <(buildah images --format '{{.Name}}:{{.Tag}}' 2>/dev/null)
  /usr/bin/python3 "$ART/evidence.py" clean "$ART/processes-before-cleanup.json" "$WORK" > "$ART/cleanup-evidence.txt" 2>&1 || cleanup_failed=1
  if compgen -G "$WORK/i/*.json" >/dev/null; then cleanup_failed=1; fi
  if compgen -G "$WORK/s/*" >/dev/null; then cleanup_failed=1; fi
  if [[ -d $WAYPOINT_TMPFS_DIR ]] && [[ -n $(ls -A "$WAYPOINT_TMPFS_DIR") ]]; then cleanup_failed=1; fi
  [[ "$(cat /proc/sys/kernel/hostname)" == "$HOST_HOSTNAME" ]] || cleanup_failed=1
  [[ "$(stat -Lc '%F %t:%T %a' /dev/ptmx) $(readlink /dev/ptmx || true)" == "$HOST_PTMX" ]] || cleanup_failed=1
  if [[ -f $ART/source-before.json ]]; then
    /usr/bin/python3 "$ART/evidence.py" hash "$ART/preparation" > "$ART/source-after.json"
    cmp -s "$ART/source-before.json" "$ART/source-after.json" || cleanup_failed=1
  fi
  if [[ -f $ART/rootfs-before.json ]]; then
    /usr/bin/python3 "$ART/evidence.py" hash "$IMAGE_CONTEXT" > "$ART/rootfs-after.json"
    cmp -s "$ART/rootfs-before.json" "$ART/rootfs-after.json" || cleanup_failed=1
  fi
  if (( cleanup_failed == 0 )); then
    PASS=$((PASS+1)); printf 'PASS\tCleanup removed private sessions, processes, mounts and tags; host inputs/device/hostname unchanged\n' | tee -a "$ART/assertions.tsv"
    rm -rf "$WORK"
    rmdir "$WAYPOINT_TMPFS_DIR" 2>/dev/null || true
  else
    rc=1
    printf 'FAIL\tCleanup or host integrity check; retained private work %s\n' "$WORK" | tee -a "$ART/assertions.tsv"
  fi
  [[ $COMPLETED == 1 ]] || rc=1
  /usr/bin/python3 - "$ART" "$BRANCHES" "$GENERATIONS" "$PASS" "$rc" "$PHASE" "$STARTED" "$WORK" <<'PY'
import datetime,json,os,sys
p,b,g,n,rc,phase,start,work=sys.argv[1:]
j=dict(branches=int(b),generations=int(g),assertions_passed=int(n),exit_code=int(rc),completed=int(rc)==0,last_phase=phase,started_at=start,finished_at=datetime.datetime.now(datetime.timezone.utc).isoformat(),private_work=work,tmpfs_images=os.environ['WAYPOINT_TMPFS_IMAGES'],phase_stats=os.environ['WAYPOINT_PHASE_STATS'])
open(p+'/summary.json','w').write(json.dumps(j,indent=2)+'\n')
PY
  printf '\nResult: exit=%s, passed=%s; artifacts: %s\n' "$rc" "$PASS" "$ART"
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

section 'Build local binaries and offline Python rootfs'
for tool in buildah criu ldd /usr/bin/python3; do command -v "$tool" >/dev/null; done
if [[ -z ${WAYPOINT_BIN:-} || -z ${BASH_INIT_BIN:-} ]]; then
  GO="$(command -v go || true)"
  [[ -n $GO ]] || GO=/usr/local/go/bin/go
  [[ -x $GO ]] || { echo 'Go not found; provide WAYPOINT_BIN and BASH_INIT_BIN.'; exit 1; }
  if [[ -z ${WAYPOINT_BIN:-} ]]; then (cd "$REPO" && env GOCACHE="${GOCACHE:-/tmp/waypoint-go-cache}" "$GO" build -o "$W" ./cmd/waypoint); fi
  if [[ -z ${BASH_INIT_BIN:-} ]]; then (cd "$REPO" && env GOCACHE="${GOCACHE:-/tmp/waypoint-go-cache}" CGO_ENABLED=0 "$GO" build -o "$HELPER" ./cmd/bash-init); fi
fi
W="$(readlink -f "$W")"; HELPER="$(readlink -f "$HELPER")"
export WAYPOINT_BASH_INIT_SRC="$HELPER"
check 'Both supplied or built runtime binaries are executable' test -x "$W"
check 'bash_init helper is executable' test -x "$HELPER"
wp version > "$ART/version.txt"
ROOTFS="$IMAGE_CONTEXT/rootfs"
mkdir -p "$ROOTFS"/{bin,usr/bin,usr/lib,tmp,proc,sys,root,workspace}
for command in bash cat sleep mkdir rm; do cp -L "$(command -v "$command")" "$ROOTFS/bin/$command"; done
cp -L /usr/bin/python3 "$ROOTFS/usr/bin/python3"
PY_STDLIB="$(/usr/bin/python3 -c 'import sysconfig; print(sysconfig.get_path("stdlib"))')"
cp -a "$PY_STDLIB" "$ROOTFS/usr/lib/"
for binary in "$ROOTFS"/bin/* "$ROOTFS/usr/bin/python3"; do
  while IFS= read -r library; do
    [[ -f $library ]] || continue
    mkdir -p "$ROOTFS$(dirname "$library")"
    cp -L "$library" "$ROOTFS$library"
  done < <(ldd "$binary" 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i ~ /^\//) print $i}')
done
cat > "$IMAGE_CONTEXT/Dockerfile" <<'DOCKER'
FROM scratch
COPY rootfs/ /
ENV WORKFLOW_IMAGE=offline-python PYTHONDONTWRITEBYTECODE=1
WORKDIR /workspace
RUN ["/bin/bash", "-c", "printf 'dockerfile-run-ok\\n' > /workspace/image-built.txt"]
DOCKER
/usr/bin/python3 "$ART/evidence.py" hash "$IMAGE_CONTEXT" > "$ART/rootfs-before.json"
cp "$IMAGE_CONTEXT/Dockerfile" "$ART/Dockerfile"

# The task is to normalize user-provided integer strings: ignore invalid
# values, eliminate duplicates and sort. Candidate agents apply incremental
# repairs; the evaluator reports real test scores and rejected syntax errors.
cat > "$ART/preparation/project/solution.py" <<'PY'
FLAGS = 0
def normalize(values):
    result = []
    for value in values:
        try:
            number = int(value)
        except (ValueError, TypeError):
            if FLAGS & 1:
                continue
            raise
        if FLAGS & 2 and number in result:
            continue
        result.append(number)
    return sorted(result) if FLAGS & 4 else result
PY
cat > "$ART/preparation/project/evaluate.py" <<'PY'
import json,sys
from solution import normalize
cases=[([],[]),(['1'],[1]),(['bad','2'],[2]),(['bad'],[]),(['2','2'],[2]),(['3','1'],[1,3]),([' 4 ','-2','4'],[-2,4]),(['3','bad','-1','3'],[-1,3])]
score=0
for values,expected in cases:
    try: score += normalize(values)==expected
    except (ValueError,TypeError): pass
result={'score':score,'total':len(cases)}
open('score.json','w').write(json.dumps(result)+'\n')
print(json.dumps(result))
PY
cat > "$ART/preparation/project/candidate.py" <<'PY'
import sys
from pathlib import Path
flags,candidate,generation=map(int,sys.argv[1:])
p=Path('solution.py'); s=p.read_text().splitlines()
s[0]=f'FLAGS = {flags}'
if candidate==1: s.append('deliberately invalid candidate syntax !!!')
p.write_text('\n'.join(s)+'\n')
Path('candidate.txt').write_text(f'g{generation}-c{candidate}\n')
# Every branch modifies the same input path, making accidental shared writes
# visible when independently checked and copied back after all agents finish.
Path('data/private.txt').write_text(f'g{generation}-c{candidate}\n')
PY
printf 'Preparation payload with spaces\n' > "$ART/preparation/note with spaces.txt"
chmod 0641 "$ART/preparation/note with spaces.txt"
printf 'original input\n' > "$ART/preparation/project/data/private.txt"
/usr/bin/python3 "$ART/evidence.py" hash "$ART/preparation" > "$ART/source-before.json"

section 'Dockerfile build, preparation copies, warm process state and checkpoints'
OUT="$(wp build "$IMAGE_CONTEXT" --quiet)"; printf '%s\n' "$OUT"
SESSION="${OUT%%,*}"
check 'Build returns registered session under the private registry' test -f "$WORK/i/$SESSION.json"
printf '%s\n' "$SESSION" > "$ART/session.txt"
OUT="$(wp exec "$SESSION" main -- 'cat /workspace/image-built.txt; printf "cwd=%s\n" "$PWD"')"
check 'Dockerfile RUN and initial WORKDIR reach the live shell' test "$OUT" = $'dockerfile-run-ok\ncwd=/workspace'
wp checkpoint "$SESSION" boot
wp cp "$SESSION" "$ART/preparation/project" main:/workspace/project
wp cp "$SESSION" "$ART/preparation/note with spaces.txt" 'main:/workspace/note with spaces.txt'
OUT="$(wp exec "$SESSION" main -- 'cd /workspace/project; WARM_TOKEN=prepared-memory; function warm_function { printf "warm-function-ok\n"; }; sleep 1800 & python3 evaluate.py; printf "image=%s cwd=%s token=%s\n" "$WORKFLOW_IMAGE" "$PWD" "$WARM_TOKEN"; cat "/workspace/note with spaces.txt"')"
check 'Dockerfile ENV/WORKDIR and copied preparation are usable' test "${OUT#*image=offline-python cwd=/workspace/project token=prepared-memory}" != "$OUT"
check 'Preparation file with spaces survived cp' test "${OUT#*Preparation payload with spaces}" != "$OUT"
wp cp "$SESSION" 'main:/workspace/note with spaces.txt' "$ART/export/note with spaces.txt"
check 'Copied preparation file preserves contents and mode through both directions' cmp -s "$ART/preparation/note with spaces.txt" "$ART/export/note with spaces.txt"
check 'Copied preparation file preserves mode 0641' test "$(stat -c %a "$ART/export/note with spaces.txt")" = 641
wp checkpoint "$SESSION" prepared
wp info "$SESSION" prepared > "$ART/info-prepared.json"
check 'Checkpoint info resolves a custom session registry' test -s "$ART/info-prepared.json"
wp list "$SESSION" --json > "$ART/list-prepared.json"
/usr/bin/python3 "$ART/evidence.py" hash "$WORK/s/$SESSION/checkpoints/prepared/upper/workspace/project" > "$ART/base-layer-before.json"

BASE=prepared; FLAGS=0; LAST_WINNER=''; FIRST_WINNER=''
for ((generation=1; generation<=GENERATIONS; generation++)); do
  section "Generation $generation: concurrently materialize $BRANCHES candidates from $BASE"
  pids=()
  started="$(date +%s%N)"
  for ((candidate=1; candidate<=BRANCHES; candidate++)); do
    wp fork "$SESSION" "$BASE" --id "g${generation}-c$candidate" > "$ART/fork-g$generation-c$candidate.log" &
    pids+=("$!")
  done
  worker_failed=0
  for pid in "${pids[@]}"; do wait "$pid" || worker_failed=1; done
  pids=()
  check "Generation $generation concurrent fork commands succeeded" test "$worker_failed" -eq 0
  printf '%s\tfork\t%s\n' "$generation" "$((($(date +%s%N)-started)/1000000))" >> "$ART/batch-timings.tsv"
  if (( generation <= 3 )); then NEXT_FLAGS=$((FLAGS | (1 << (generation-1)))); else NEXT_FLAGS=7; fi
  section "Generation $generation: execute and score candidates concurrently"
  pids=(); started="$(date +%s%N)"
  for ((candidate=1; candidate<=BRANCHES; candidate++)); do
    (
      fork="g${generation}-c$candidate"; mask=$FLAGS
      if ((candidate % 4 == 0 || candidate == BRANCHES)); then mask=$NEXT_FLAGS; fi
      rc=0
      wp exec "$SESSION" "$fork" -- "test \"\$WARM_TOKEN\" = prepared-memory && test \"\$PWD\" = /workspace/project && warm_function && python3 candidate.py $mask $candidate $generation && sleep 1 && python3 evaluate.py" > "$ART/eval-$fork.log" || rc=$?
      printf '%s\n' "$rc" > "$ART/exit-$fork.txt"
      printf '%s\n' "$mask" > "$ART/mask-$fork.txt"
    ) &
    pids+=("$!")
  done
  worker_failed=0
  for pid in "${pids[@]}"; do wait "$pid" || worker_failed=1; done
  pids=()
  check "Generation $generation candidate result collection completed" test "$worker_failed" -eq 0
  printf '%s\texec\t%s\n' "$generation" "$((($(date +%s%N)-started)/1000000))" >> "$ART/batch-timings.tsv"
  best=-1; winner=''
  for ((candidate=1; candidate<=BRANCHES; candidate++)); do
    fork="g${generation}-c$candidate"; rc="$(cat "$ART/exit-$fork.txt")"; mask="$(cat "$ART/mask-$fork.txt")"
    if ((candidate==1)); then
      check "Generation $generation malformed candidate reports nonzero status" test "$rc" -ne 0
      printf '%s\t%s\t%s\t0\trejected\n' "$generation" "$candidate" "$mask" >> "$ART/scores.tsv"
      OUT="$(wp exec "$SESSION" "$fork" -- 'printf "rejected-branch-still-usable\n"')"
      check "Generation $generation rejected candidate shell remains usable" test "$OUT" = rejected-branch-still-usable
    else
      check "$fork executes with inherited memory/cwd/function and evaluates successfully" test "$rc" -eq 0
      wp cp "$SESSION" "$fork:/workspace/project/score.json" "$ART/export/score-$fork.json"
      score="$(/usr/bin/python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["score"])' "$ART/export/score-$fork.json")"
      printf '%s\t%s\t%s\t%s\taccepted\n' "$generation" "$candidate" "$mask" "$score" >> "$ART/scores.tsv"
      if ((score>best)); then best=$score; winner=$fork; fi
    fi
    OUT="$(wp exec "$SESSION" "$fork" -- 'cat candidate.txt data/private.txt')"
    check "$fork sees only its own edits after all concurrent candidates finish" test "$OUT" = "$fork"$'\n'"$fork"
  done
  check "Generation $generation selected a scored winner" test -n "$winner"
  printf '%s\t%s\t%s\n' "$generation" "$winner" "$best" >> "$ART/winners.tsv"
  wp snapshot "$SESSION" "$winner" "winner-$generation"
  wp info "$SESSION" "winner-$generation" > "$ART/info-winner-$generation.json"
  /usr/bin/python3 "$ART/evidence.py" hash "$WORK/s/$SESSION/checkpoints/winner-$generation/upper/workspace/project" > "$ART/winner-$generation-before.json"
  [[ -n $FIRST_WINNER ]] || FIRST_WINNER=$winner
  LAST_WINNER=$winner; FLAGS=$NEXT_FLAGS; BASE="winner-$generation"
  # Losers are discarded during search. Winner forks stay live to check that
  # subsequent generations and backtracking do not modify earlier results.
  for ((candidate=1; candidate<=BRANCHES; candidate++)); do
    fork="g${generation}-c$candidate"
    [[ $fork == "$winner" ]] || wp destroy "$SESSION" "$fork" > "$ART/destroy-$fork.log"
  done
  check "Generation $generation losers can be destroyed after rejection or evaluation" true
 done

section 'Backtrack to an earlier checkpoint through new forks and export final result'
wp fork "$SESSION" prepared --id backtrack
OUT="$(wp exec "$SESSION" backtrack -- 'test ! -f candidate.txt && cat data/private.txt; python3 evaluate.py; printf "token=%s\n" "$WARM_TOKEN"')"
check 'Backtracking sees original file and memory state' test "${OUT#*original input}" != "$OUT"
check 'Backtracking retains warm shell variables' test "${OUT#*token=prepared-memory}" != "$OUT"
wp fork "$SESSION" winner-1 --id revisit-winner
OUT="$(wp exec "$SESSION" revisit-winner -- 'cat candidate.txt data/private.txt')"
check 'First winner checkpoint stays independent of later descendants' test "$OUT" = "$FIRST_WINNER"$'\n'"$FIRST_WINNER"
wp exec "$SESSION" backtrack -- 'printf "backtracked alternate\n" > alternate.txt'
wp snapshot "$SESSION" backtrack alternate
wp fork "$SESSION" alternate --id alternate-child
OUT="$(wp exec "$SESSION" alternate-child -- 'cat alternate.txt; test ! -f candidate.txt')"
check 'Backtracked branch can checkpoint and recursively fork its own alternative' test "$OUT" = 'backtracked alternate'
wp cp "$SESSION" "$LAST_WINNER:/workspace/project" "$ART/export/final-project"
wp cp "$SESSION" "$LAST_WINNER:/workspace/project/solution.py" "$ART/export/final-solution.py"
check 'Copied final file agrees with copied directory' cmp -s "$ART/export/final-solution.py" "$ART/export/final-project/solution.py"
# Host execution uses only our deterministic test fixture, and never Waypoint.
(cd "$ART/export/final-project" && /usr/bin/python3 evaluate.py) > "$ART/host-evaluation.json"
EXPECTED_FLAGS=$(((1 << (GENERATIONS<3 ? GENERATIONS : 3))-1))
check 'Exported implementation exactly matches the selected repair' /usr/bin/python3 - "$ART/export/final-solution.py" "$EXPECTED_FLAGS" "$ART/preparation/project/solution.py" <<'PY'
import sys
expected=open(sys.argv[3]).read().replace('FLAGS = 0',f'FLAGS = {sys.argv[2]}',1)
assert open(sys.argv[1]).read()==expected
PY
if ((GENERATIONS>=3)); then
  check 'Final exported solution passes all eight real Python cases on the host' /usr/bin/python3 - "$ART/host-evaluation.json" <<'PY'
import json,sys
r=json.load(open(sys.argv[1])); assert r['score']==r['total']==8,r
PY
fi
wp list "$SESSION" --json > "$ART/final-list.json"
check 'Checkpoint DAG records winner ancestry' /usr/bin/python3 "$ART/evidence.py" dag "$ART/final-list.json" "$GENERATIONS"
/usr/bin/python3 "$ART/evidence.py" hash "$WORK/s/$SESSION/checkpoints/prepared/upper/workspace/project" > "$ART/base-layer-after.json"
check 'Sealed preparation layer is immutable after all search activity' cmp -s "$ART/base-layer-before.json" "$ART/base-layer-after.json"
for ((generation=1; generation<=GENERATIONS; generation++)); do
  /usr/bin/python3 "$ART/evidence.py" hash "$WORK/s/$SESSION/checkpoints/winner-$generation/upper/workspace/project" > "$ART/winner-$generation-after.json"
  check "Winner $generation sealed source files are immutable" cmp -s "$ART/winner-$generation-before.json" "$ART/winner-$generation-after.json"
done
expect_failure 'Missing checkpoint reports a failure without breaking active candidates' fork "$SESSION" missing --id missing-checkpoint
OUT="$(wp exec "$SESSION" "$LAST_WINNER" -- 'printf "winner-still-usable\n"')"
check 'Final winner remains usable after rejected control command' test "$OUT" = winner-still-usable
COMPLETED=1
section 'Cleanup and host integrity checks'
