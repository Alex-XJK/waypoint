#!/bin/bash

# Usage: sudo ./inject_and_capture.sh [--no-capture] <PID> "<COMMAND>"
#   --no-capture  Inject without capturing output (for exports, bg jobs, etc.)
#
# This script injects commands into a running shell (even in a sandbox/chroot)
# and captures the output. It uses /proc/PID/root to access sandboxed filesystems.

NO_CAPTURE=0

# Parse optional flag
if [ "$1" = "--no-capture" ]; then
    NO_CAPTURE=1
    shift
fi

PID=$1
CMD=$2

if [ -z "$PID" ] || [ -z "$CMD" ]; then
    echo "Usage: sudo $0 [--no-capture] <PID> \"<COMMAND>\""
    exit 1
fi

# 1. Find the TTY for the given PID
# We check fd 0 (stdin) to find the controlling terminal
TTY_PATH=$(readlink /proc/"$PID"/fd/0)

if [ ! -c "$TTY_PATH" ]; then
    echo "Error: Could not find valid TTY for PID $PID (found: $TTY_PATH)"
    exit 1
fi

# Fast path: no capture, just inject
if [ "$NO_CAPTURE" -eq 1 ]; then
    python3 -c '
import fcntl, termios, sys, os
tty_path = sys.argv[1]
cmd = sys.argv[2] + "\n"
try:
    fd = os.open(tty_path, os.O_WRONLY)
    for ch in cmd:
        fcntl.ioctl(fd, termios.TIOCSTI, ch)
    os.close(fd)
except Exception as e:
    print(f"Injection failed: {e}")
    sys.exit(1)
' "$TTY_PATH" "$CMD"
    exit $?
fi

# 2. Setup temporary file paths
# Files will be created inside the target process's filesystem (possibly a sandbox)
# We access them via /proc/PID/root/ which shows the process's root filesystem
OUT_FILE="/tmp/inject_out.$$.$RANDOM"
DONE_FILE="/tmp/inject_done.$$.$RANDOM"

# Path to access files from host (works for both normal and sandboxed processes)
HOST_OUT_FILE="/proc/$PID/root$OUT_FILE"
HOST_DONE_FILE="/proc/$PID/root$DONE_FILE"

# 3. Construct the payload
# We use 'exec' to redirect stdout/stderr of the current shell temporarily
# so we capture output without spawning a subshell.
# Note: Commands run in the target's environment (sandbox), output goes to its /tmp
PAYLOAD=" exec 3>&1 4>&2 >$OUT_FILE 2>&1; $CMD; exec 1>&3 2>&4 3>&- 4>&-; echo done > $DONE_FILE"

# 4. Inject the command using Python and TIOCSTI
python3 -c '
import fcntl, termios, sys, os
tty_path = sys.argv[1]
payload = sys.argv[2] + "\n"
try:
    fd = os.open(tty_path, os.O_WRONLY)
    for ch in payload:
        fcntl.ioctl(fd, termios.TIOCSTI, ch)
    os.close(fd)
except Exception as e:
    print(f"Injection failed: {e}")
    sys.exit(1)
' "$TTY_PATH" "$PAYLOAD"

if [ $? -ne 0 ]; then
    echo "Error: Failed to inject command."
    exit 1
fi

# 5. Wait for the command to complete
# We poll for the done file via /proc/PID/root (works with sandboxes)
TIMEOUT=10
START_TIME=$(date +%s)

while [ ! -f "$HOST_DONE_FILE" ]; do
    CURRENT_TIME=$(date +%s)
    ELAPSED=$((CURRENT_TIME - START_TIME))
    
    if [ "$ELAPSED" -ge "$TIMEOUT" ]; then
        echo "Error: Timed out waiting for command to complete."
        break
    fi
    sleep 0.1
done

# 6. Print the captured output (via /proc/PID/root for sandbox support)
if [ -f "$HOST_OUT_FILE" ]; then
    cat "$HOST_OUT_FILE"
fi

# 7. Cleanup (via /proc/PID/root)
rm -f "$HOST_OUT_FILE" "$HOST_DONE_FILE" 2>/dev/null
