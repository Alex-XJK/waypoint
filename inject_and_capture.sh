#!/bin/bash

# Usage: sudo ./inject_and_capture.sh [--no-capture] <PID> "<COMMAND>"
#
# Options:
#   --no-capture    Just inject the command without capturing output.
#                   Use for background jobs, exports, cd, etc.
#
# Examples:
#   sudo ./inject_and_capture.sh 12345 "ls -la"                    # With output capture
#   sudo ./inject_and_capture.sh --no-capture 12345 "sleep 10 &"   # No capture (background job)
#   sudo ./inject_and_capture.sh --no-capture 12345 "export FOO=bar"

NO_CAPTURE=0

# Parse --no-capture flag
if [ "$1" = "--no-capture" ]; then
    NO_CAPTURE=1
    shift
fi

PID=$1
CMD=$2

if [ -z "$PID" ] || [ -z "$CMD" ]; then
    echo "Usage: sudo $0 [--no-capture] <PID> \"<COMMAND>\""
    echo ""
    echo "Options:"
    echo "  --no-capture    Just inject without capturing output"
    exit 1
fi

# 1. Find the TTY for the given PID
TTY_PATH=$(readlink /proc/"$PID"/fd/0)

if [ ! -c "$TTY_PATH" ]; then
    echo "Error: Could not find valid TTY for PID $PID (found: $TTY_PATH)"
    exit 1
fi

# 2. Handle --no-capture mode (simple injection)
if [ "$NO_CAPTURE" -eq 1 ]; then
    python3 -c '
import fcntl, termios, sys, os

tty_path = sys.argv[1]
cmd = sys.argv[2] + "\n"

try:
    fd = os.open(tty_path, os.O_WRONLY)
    for char in cmd:
        fcntl.ioctl(fd, termios.TIOCSTI, char)
    os.close(fd)
except Exception as e:
    print(f"Injection failed: {e}")
    sys.exit(1)
' "$TTY_PATH" "$CMD"
    exit $?
fi

# 3. Setup temporary file paths for output capture mode
OUT_FILE="/tmp/inject_out.$$.$RANDOM"
DONE_FILE="/tmp/inject_done.$$.$RANDOM"

# 4. Construct the payload with exec-based redirection
# This runs the command in the current shell context (preserves exports, etc.)
PAYLOAD=" rm -f $OUT_FILE $DONE_FILE; exec 3>&1 4>&2 >$OUT_FILE 2>&1; $CMD; exec 1>&3 2>&4 3>&- 4>&-; echo done > $DONE_FILE"

# 5. Inject the command using Python and TIOCSTI
python3 -c '
import fcntl, termios, sys, os

tty_path = sys.argv[1]
payload = sys.argv[2] + "\n"

try:
    fd = os.open(tty_path, os.O_WRONLY)
    for char in payload:
        fcntl.ioctl(fd, termios.TIOCSTI, char)
    os.close(fd)
except Exception as e:
    print(f"Injection failed: {e}")
    sys.exit(1)
' "$TTY_PATH" "$PAYLOAD"

if [ $? -ne 0 ]; then
    echo "Error: Failed to inject command."
    rm -f "$OUT_FILE" "$DONE_FILE"
    exit 1
fi

# 6. Wait for the command to complete (timeout after 10 seconds)
TIMEOUT=10
START_TIME=$(date +%s)

while [ ! -f "$DONE_FILE" ]; do
    CURRENT_TIME=$(date +%s)
    ELAPSED=$((CURRENT_TIME - START_TIME))
    
    if [ "$ELAPSED" -ge "$TIMEOUT" ]; then
        echo "Error: Timed out waiting for command to complete."
        break
    fi
    sleep 0.1
done

# 7. Print the captured output
if [ -f "$OUT_FILE" ]; then
    cat "$OUT_FILE"
fi

# 8. Cleanup
rm -f "$OUT_FILE" "$DONE_FILE"