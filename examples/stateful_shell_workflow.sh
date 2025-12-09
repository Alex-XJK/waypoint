#!/bin/bash
# Example workflow for using checkpoint-lite with stateful shell sessions
# This demonstrates how to maintain shell state (environment variables, etc.) across checkpoints

# ==============================================================================
# TERMINAL A: Start a stateful shell session
# ==============================================================================
# Start a clean bash shell using script command (run this manually in Terminal A)
# script -q -c "bash --norc --noprofile" /dev/null

# ==============================================================================
# TERMINAL B: Controller - Initialize and manage checkpoints
# ==============================================================================

# Step 1: Initialize checkpoint-lite environment
SESSION_INFO=$(./checkpoint-lite init /tmp/test-workspace --quiet)
SESSION_ID=$(echo $SESSION_INFO | cut -d',' -f1)
WORK_DIR=$(echo $SESSION_INFO | cut -d',' -f2)

echo "Session ID: $SESSION_ID"
echo "Work Directory: $WORK_DIR"

# Step 2: Get the PID of the bash shell started in Terminal A
TARGET_PID=$(pgrep -n bash)
echo "Target Shell PID: $TARGET_PID"

# Step 3: Inject initial state into the shell
./checkpoint-lite inject $SESSION_ID $TARGET_PID 'export MY_VAR=InitialState'
./checkpoint-lite inject $SESSION_ID $TARGET_PID 'export COUNTER=1'
./checkpoint-lite inject $SESSION_ID $TARGET_PID 'echo MY_VAR=$MY_VAR COUNTER=$COUNTER'

# Step 4: Create Checkpoint 1
./checkpoint-lite create $SESSION_ID $TARGET_PID ckpt1
echo "Checkpoint 1 created"

# ==============================================================================
# TERMINAL C: Restore Checkpoint 1 (run this in a new terminal)
# ==============================================================================
# ./checkpoint-lite restore <SESSION_ID> ckpt1

# ==============================================================================
# Back to TERMINAL B: Continue modifying state
# ==============================================================================

# Step 5: Verify state was preserved
./checkpoint-lite inject $SESSION_ID $TARGET_PID 'echo MY_VAR=$MY_VAR COUNTER=$COUNTER'
# Expected output: MY_VAR=InitialState COUNTER=1

# Step 6: Modify state
./checkpoint-lite inject $SESSION_ID $TARGET_PID 'export MY_VAR=ModifiedState'
./checkpoint-lite inject $SESSION_ID $TARGET_PID 'export COUNTER=2'
./checkpoint-lite inject $SESSION_ID $TARGET_PID 'echo MY_VAR=$MY_VAR COUNTER=$COUNTER'
# Expected output: MY_VAR=ModifiedState COUNTER=2

# Step 7: Create Checkpoint 2
./checkpoint-lite create $SESSION_ID $TARGET_PID ckpt2
echo "Checkpoint 2 created"

# ==============================================================================
# TERMINAL D: Restore Checkpoint 2 (run this in a new terminal)
# ==============================================================================
# ./checkpoint-lite restore <SESSION_ID> ckpt2

# ==============================================================================
# Back to TERMINAL B: Verify final state
# ==============================================================================

# Step 8: Verify state in checkpoint 2
./checkpoint-lite inject $SESSION_ID $TARGET_PID 'echo MY_VAR=$MY_VAR COUNTER=$COUNTER'
# Expected output: MY_VAR=ModifiedState COUNTER=2

# Step 9: List all checkpoints
./checkpoint-lite list $SESSION_ID

# Step 10: Cleanup when done
./checkpoint-lite cleanup $SESSION_ID --force

# ==============================================================================
# Notes:
# ==============================================================================
# 1. The inject command requires inject_and_capture.sh to be in the same directory
#    as checkpoint-lite or in /usr/local/bin/
# 
# 2. Run checkpoint-lite as root or with sudo to enable CRIU operations
# 
# 3. The stateful shell workflow maintains:
#    - Environment variables (export)
#    - Shell functions
#    - Working directory
#    - Command history
#    - Any other shell state
# 
# 4. Each checkpoint captures both:
#    - Memory state (CRIU dump of the bash process)
#    - Filesystem state (OverlayFS snapshot)
