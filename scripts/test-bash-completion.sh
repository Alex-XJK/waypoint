#!/usr/bin/env bash
# Exercise completion without a real session, CRIU, root, or guest commands.
set -eo pipefail

repo_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
completion_file="$repo_dir/contrib/bash-completion/waypoint"

# Run in fresh shells so both the standalone fallback and installed helpers are
# exercised. The test also accepts an explicit bash-completion framework path.
if [[ ${1-} != --mode ]]; then
    bash --noprofile --norc "$0" --mode standalone
    framework=${BASH_COMPLETION_TEST_FRAMEWORK-}
    if [[ -z $framework ]]; then
        for candidate in /usr/share/bash-completion/bash_completion /etc/bash_completion; do
            if [[ -r $candidate ]]; then
                framework=$candidate
                break
            fi
        done
    fi
    if [[ -n $framework ]]; then
        bash --noprofile --norc "$0" --mode framework "$framework"
    else
        printf 'SKIP: bash-completion framework is not installed\n'
    fi
    exit
fi

mode=$2
if [[ $mode == framework ]]; then
    # bash-completion assumes interactive-shell defaults, including no errexit.
    set +e
    source "$3"
    set -e
fi
source "$completion_file"

test_dir=$(mktemp -d)
trap 'rm -rf -- "$test_dir"' EXIT
mkdir -p "$test_dir/bin" "$test_dir/explicit path" "$test_dir/work/host dir" "$test_dir/work/plain-dir"
touch "$test_dir/work/host file" "$test_dir/work/plain-file" "$test_dir/work/image:latest"
export WAYPOINT_COMPLETION_TEST_CALLS="$test_dir/calls"
export WAYPOINT_COMPLETION_TEST_MODE=normal
: > "$WAYPOINT_COMPLETION_TEST_CALLS"
cat > "$test_dir/bin/waypoint" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$WAYPOINT_COMPLETION_TEST_CALLS"
if [[ $1 != list || $# -gt 2 ]]; then
    printf 'UNEXPECTED invocation: %s\n' "$*" >> "$WAYPOINT_COMPLETION_TEST_CALLS"
    exit 97
fi
if [[ $WAYPOINT_COMPLETION_TEST_MODE == malformed ]]; then
    printf 'Unexpected status report:\n  bogus parent= status=ready layers=bogus\n  bogus checkpoint= status=running pid=1 socket=/tmp/bogus.sock\n'
    exit 0
fi
if [[ $WAYPOINT_COMPLETION_TEST_MODE == error || ${2-} == failed-session ]]; then
    printf 'Error loading session: inaccessible registry\n'
    printf 'diagnostic on stderr\n' >&2
    exit 1
fi
if [[ $# == 1 ]]; then
    if [[ $WAYPOINT_COMPLETION_TEST_MODE == empty ]]; then
        printf 'No sessions found\n'
    else
        printf 'Available sessions:\n  demo-session\n  other-session\n  empty-session\n  failed-session\n'
    fi
elif [[ $2 == empty-session ]]; then
    printf 'No checkpoints found\n'
elif [[ $2 == other-session ]]; then
    printf 'No checkpoints found\nLive forks:\n  main checkpoint= status=running pid=123 socket=/tmp/main.sock\n'
elif [[ $2 == demo-session ]]; then
    cat <<'LISTING'
Checkpoints:
  c-alpha parent= status=ready layers=c-alpha
  c.beta parent=c-alpha status=ready layers=c-alpha,c.beta
Live forks:
  main checkpoint=c-alpha status=running pid=123 socket=/tmp/main.sock
  worker-1 checkpoint=c.beta status=running pid=124 socket=/tmp/worker.sock
  idle-2 checkpoint=c-alpha status=stopped pid=0 socket=/tmp/idle.sock
LISTING
else
    printf 'UNEXPECTED session: %s\n' "$2" >> "$WAYPOINT_COMPLETION_TEST_CALLS"
    exit 98
fi
STUB
chmod +x "$test_dir/bin/waypoint"
cp "$test_dir/bin/waypoint" "$test_dir/explicit path/waypoint"
export PATH="$test_dir/bin:$PATH"
cd "$test_dir/work"

checks=0
fail() {
    printf 'FAIL (%s): %s\n  input: %s\n' "$mode" "$*" "$COMP_LINE" >&2
    printf '  replies:' >&2
    printf ' <%s>' "${COMPREPLY[@]}" >&2
    printf '\n' >&2
    exit 1
}

# Pass the actual line separately for Bash's default colon word splitting.
complete_line() {
    COMP_LINE=$1
    shift
    COMP_WORDS=("$@")
    COMP_CWORD=$((${#COMP_WORDS[@]} - 1))
    COMP_POINT=${#COMP_LINE}
    COMP_TYPE=9
    COMP_KEY=9
    COMPREPLY=()
    _waypoint > "$test_dir/stdout" 2> "$test_dir/stderr" || :
    [[ ! -s $test_dir/stdout && ! -s $test_dir/stderr ]] || fail 'completion leaked output'
    checks=$((checks + 1))
}

complete_words() {
    local line= word quoted
    local -a shell_words=()
    for word in "$@"; do
        if [[ -n $word ]]; then
            printf -v quoted '%q' "$word"
        else
            quoted=
        fi
        line+="${line:+ }$quoted"
        shell_words+=("$quoted")
    done
    complete_line "$line" "${shell_words[@]}"
}

contains() {
    local wanted=$1 reply
    for reply in "${COMPREPLY[@]}"; do
        [[ $reply == "$wanted" ]] && return 0
    done
    fail "missing candidate: $wanted"
}

excludes() {
    local unwanted=$1 reply
    for reply in "${COMPREPLY[@]}"; do
        [[ $reply != "$unwanted" ]] || fail "unexpected candidate: $unwanted"
    done
}

empty() {
    [[ ${#COMPREPLY[@]} == 0 ]] || fail 'expected no candidates'
}

exactly() {
    [[ ${#COMPREPLY[@]} == $# ]] || fail "expected $# candidates"
    local expected
    for expected in "$@"; do contains "$expected"; done
}

has_path() {
    local expected=$1 reply
    for reply in "${COMPREPLY[@]}"; do
        [[ ${reply%/} == "$expected" ]] && return 0
    done
    fail "missing intact path: $expected"
}

complete_words waypoint ''
for command in init build checkpoint create fork exec fork-exec cp snapshot destroy list suspend cleanup info version; do
    contains "$command"
done
for command in restore delete run __waypoint_restore_fork_child __waypoint_flush_images; do
    excludes "$command"
done
complete_words waypoint --v
contains --version
complete_words waypoint -v
contains -v
complete_words waypoint ver
exactly version
for command in version --version -v unknown-command; do
    complete_words waypoint "$command" ''
    empty
done

# Session lookup is shared by all session commands, including legacy aliases.
for command in checkpoint create fork exec fork-exec cp snapshot destroy list suspend cleanup info; do
    complete_words waypoint "$command" demo
    exactly demo-session
done
complete_words waypoint info ''
exactly demo-session other-session empty-session failed-session
complete_words waypoint list --
empty # --json follows a session; it is not a global list flag.
complete_words waypoint cleanup --
empty
complete_words waypoint fork demo-session --
empty

# Query the executable the user invoked, including a path containing spaces.
# A failing PATH entry makes accidentally hard-coding `waypoint` observable.
mkdir "$test_dir/wrong-bin"
printf '#!/usr/bin/env bash\nexit 1\n' > "$test_dir/wrong-bin/waypoint"
chmod +x "$test_dir/wrong-bin/waypoint"
saved_path=$PATH
PATH="$test_dir/wrong-bin:$PATH"
complete_words "$test_dir/explicit path/waypoint" fork demo-session c
exactly c-alpha c.beta
PATH=$saved_path
complete_line 'waypoint fork "demo-session" c' waypoint fork '"demo-session"' c
exactly c-alpha c.beta

for command in fork info; do
    complete_words waypoint "$command" demo-session ''
    exactly c-alpha c.beta
done
for command in exec fork-exec snapshot; do
    complete_words waypoint "$command" demo-session worker
    exactly worker-1
    complete_words waypoint "$command" demo-session idle
    empty
done
complete_words waypoint destroy demo-session ''
exactly main worker-1 idle-2
complete_words waypoint fork other-session ''
empty
complete_words waypoint exec other-session ''
exactly main

for command in checkpoint create; do
    complete_words waypoint "$command" demo-session ''
    empty
    complete_words waypoint "$command" demo-session c
    empty
done
complete_words waypoint snapshot demo-session worker-1 c
empty
complete_words waypoint fork demo-session c-alpha --id ''
empty
complete_words waypoint fork demo-session c-alpha --n ''
empty
complete_words waypoint fork demo-session c-alpha --id --
empty
complete_words waypoint fork demo-session c-alpha --n --
empty

complete_words waypoint fork demo-session c-alpha --
contains --id
contains --n
complete_words waypoint fork demo-session c-alpha --n 2 --
excludes --id
contains --n
complete_words waypoint fork demo-session c-alpha --n 1 --
contains --id
complete_words waypoint list demo-session --
exactly --json
complete_words waypoint cleanup demo-session --
exactly --force
for command in init build; do
    complete_words waypoint "$command" host
    has_path 'host dir'
    excludes 'host file'
    complete_words waypoint "$command" --
    empty
    complete_words waypoint "$command" 'host dir' --
    contains --quiet
done
complete_words waypoint init 'host dir' --
contains --shell
complete_words waypoint build 'host dir' --
excludes --shell
# Direct function calls cannot emulate Readline's quoting state. The raw
# prefixes above verify that space-containing candidates stay intact; partially
# escaped or quoted current words require a separate interactive Readline check.
# Framework file completion also needs Readline state for an empty prefix, so
# its path assertions below use the nonempty prefix 'host'.

# snapshot accepts --park before, between, or after its positional arguments.
for args in before-session after-session after-fork after-checkpoint; do
    case $args in
        before-session) complete_words waypoint snapshot -- ;;
        after-session) complete_words waypoint snapshot demo-session -- ;;
        after-fork) complete_words waypoint snapshot demo-session worker-1 -- ;;
        after-checkpoint) complete_words waypoint snapshot demo-session worker-1 new-checkpoint -- ;;
    esac
    contains --park
done
complete_words waypoint snapshot --park demo
exactly demo-session
complete_words waypoint snapshot demo-session --park ''
exactly worker-1
complete_words waypoint snapshot --park demo-session worker
exactly worker-1
complete_words waypoint snapshot demo-session worker-1 --park c
empty

# While editing an earlier operand, a trailing --park still excludes main.
COMP_LINE='waypoint snapshot demo-session main new-checkpoint --park'
COMP_WORDS=(waypoint snapshot demo-session main new-checkpoint --park)
COMP_CWORD=3
cursor_prefix='waypoint snapshot demo-session ma'
COMP_POINT=${#cursor_prefix}
COMPREPLY=()
_waypoint > "$test_dir/stdout" 2> "$test_dir/stderr" || :
[[ ! -s $test_dir/stdout && ! -s $test_dir/stderr ]] || fail 'completion leaked output'
checks=$((checks + 1))
empty

complete_words waypoint exec demo-session worker-1 ''
exactly --
complete_words waypoint exec demo-session worker-1 -- ''
empty
complete_words waypoint exec demo-session worker-1 -- cat host
empty
complete_words waypoint fork-exec demo-session worker-1 ''
empty
complete_words waypoint fork-exec demo-session worker-1 cat host
empty
for command in destroy suspend info checkpoint; do
    case $command in
        destroy) complete_words waypoint destroy demo-session main '' ;;
        suspend) complete_words waypoint suspend demo-session '' ;;
        info) complete_words waypoint info demo-session c-alpha '' ;;
        checkpoint) complete_words waypoint checkpoint demo-session new-checkpoint '' ;;
    esac
    empty
done

# Copy source offers host paths and live fork prefixes; destination must use
# the other endpoint type. Guest paths are not enumerated by running commands.
complete_words waypoint cp demo-session ''
if [[ $mode == standalone ]]; then
    has_path 'host file'
    has_path 'host dir'
fi
contains main:/
contains worker-1:/
excludes idle-2:/
complete_words waypoint cp demo-session host
has_path 'host file'
has_path 'host dir'
complete_words waypoint cp demo-session worker
exactly worker-1:/
complete_words waypoint cp demo-session 'host file' ''
exactly main:/ worker-1:/
complete_words waypoint cp demo-session '$(touch completion-pwned)' worker
exactly worker-1:/
[[ ! -e completion-pwned ]] || fail 'completion evaluated a literal operand as a command'
complete_words waypoint cp demo-session worker-1:/tmp/host host
has_path 'host file'
has_path 'host dir'
excludes main:/
excludes worker-1:/
complete_line 'waypoint cp demo-session "worker-1:/tmp/host" host' waypoint cp demo-session '"worker-1:/tmp/host"' host
has_path 'host file'
has_path 'host dir'
excludes worker-1:/
complete_words waypoint cp demo-session worker-1:/tmp/ho
empty
complete_words waypoint cp demo-session 'host file' worker-1:/tmp/ho
empty
complete_words waypoint cp demo-session worker-1:/tmp/host 'host file' ''
empty

# These arrays match readline's default COMP_WORDBREAKS treatment of ':'.
complete_line 'waypoint cp demo-session worker-1:' waypoint cp demo-session worker-1 :
exactly /
complete_line 'waypoint cp demo-session worker-1:/tmp/ho' waypoint cp demo-session worker-1 : /tmp/ho
empty
complete_line 'waypoint cp demo-session worker-1:/tmp/host host' waypoint cp demo-session worker-1 : /tmp/host host
has_path 'host file'
has_path 'host dir'
excludes worker-1:/
complete_line 'waypoint cp demo-session plain-file worker-1:' waypoint cp demo-session plain-file worker-1 :
exactly /
complete_line 'waypoint cp demo-session plain-file worker-1:/tmp/ho' waypoint cp demo-session plain-file worker-1 : /tmp/ho
empty
complete_line 'waypoint cp demo-session image:latest worker' waypoint cp demo-session image : latest worker
exactly worker-1:/

# Empty and failed list output must never leak its prose into candidates.
complete_words waypoint fork empty-session ''
empty
complete_words waypoint destroy empty-session ''
empty
complete_words waypoint fork failed-session ''
empty
WAYPOINT_COMPLETION_TEST_MODE=empty
complete_words waypoint info ''
empty
WAYPOINT_COMPLETION_TEST_MODE=error
complete_words waypoint info ''
empty
WAYPOINT_COMPLETION_TEST_MODE=malformed
complete_words waypoint info ''
empty
complete_words waypoint fork demo-session ''
empty
complete_words waypoint destroy demo-session ''
empty
WAYPOINT_COMPLETION_TEST_MODE=normal

while IFS= read -r invocation; do
    [[ $invocation != UNEXPECTED* ]] || fail 'completion invoked an unexpected command or session'
done < "$WAYPOINT_COMPLETION_TEST_CALLS"
printf 'PASS: %s (%d completion cases)\n' "$mode" "$checks"
