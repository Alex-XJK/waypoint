package checkpoint

// Command injection utilities for stateful shell sessions

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"time"
)

// findInjectScript finds the inject_and_capture.sh script
func findInjectScript() string {
	scriptPath := "./inject_and_capture.sh"
	if _, err := exec.LookPath(scriptPath); err != nil {
		scriptPath = filepath.Join("/usr/local/bin", "inject_and_capture.sh")
		if _, err := exec.LookPath(scriptPath); err != nil {
			scriptPath = "/users/gliargko/checkpoint-lite/inject_and_capture.sh"
		}
	}
	return scriptPath
}

// getLatestCheckpoint returns the most recent checkpoint ID based on timestamp
func (m *Manager) getLatestCheckpoint() (string, error) {
	checkpoints, err := m.ListCheckpoints()
	if err != nil {
		return "", err
	}
	if len(checkpoints) == 0 {
		return "", fmt.Errorf("no checkpoints available")
	}

	// Find checkpoint with latest timestamp
	var latest string
	var latestTime int64

	for _, cpID := range checkpoints {
		meta, err := m.loadMetadata(cpID)
		if err != nil {
			continue
		}
		if meta.Timestamp > latestTime {
			latestTime = meta.Timestamp
			latest = cpID
		}
	}

	if latest == "" {
		// Fallback: sort alphabetically and take last (for ckpt1, ckpt2, ckpt3...)
		sort.Strings(checkpoints)
		latest = checkpoints[len(checkpoints)-1]
	}

	return latest, nil
}

// ensureProcessRunning checks if process exists, if not restores from latest checkpoint
// Returns the (possibly new) PID of the running process
func (m *Manager) ensureProcessRunning(pid int) (int, error) {
	if m.processExists(pid) {
		return pid, nil
	}

	fmt.Printf("Process %d not running, restoring from latest checkpoint...\n", pid)

	// Get latest checkpoint
	latestCP, err := m.getLatestCheckpoint()
	if err != nil {
		return 0, fmt.Errorf("no checkpoint to restore from: %w", err)
	}

	fmt.Printf("Restoring from checkpoint '%s'...\n", latestCP)

	// Setup bind mounts before restore
	if err := SetupBindMountsForRestore(m.workOverlay); err != nil {
		fmt.Printf("Warning: failed to setup bind mounts: %v\n", err)
	}

	// Restore the checkpoint
	newPID, err := m.RestoreCheckpointNew(latestCP)
	if err != nil {
		return 0, fmt.Errorf("failed to restore checkpoint: %w", err)
	}

	// Wait a moment for process to fully start
	time.Sleep(500 * time.Millisecond)

	fmt.Printf("Restored process with PID %d\n", newPID)
	return newPID, nil
}

// InjectCommand injects a command into a running shell process using inject_and_capture.sh
// If the process is not running, it will be restored from the latest checkpoint first.
func (m *Manager) InjectCommand(pid int, command string) (string, error) {
	// Ensure process is running, restore if needed
	actualPID, err := m.ensureProcessRunning(pid)
	if err != nil {
		return "", err
	}

	scriptPath := findInjectScript()
	cmd := exec.Command(scriptPath, fmt.Sprintf("%d", actualPID), command)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("command injection failed: %w", err)
	}

	return string(output), nil
}

// InjectCommandNoCapture injects a command without waiting for output
// If the process is not running, it will be restored from the latest checkpoint first.
func (m *Manager) InjectCommandNoCapture(pid int, command string) error {
	// Ensure process is running, restore if needed
	actualPID, err := m.ensureProcessRunning(pid)
	if err != nil {
		return err
	}

	scriptPath := findInjectScript()
	cmd := exec.Command(scriptPath, "--no-capture", fmt.Sprintf("%d", actualPID), command)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("command injection failed: %w\nOutput: %s", err, string(output))
	}

	return nil
}

// StartStatefulShell starts a new stateful bash shell using script command
// Returns the PID of the bash process that should be checkpointed
func (m *Manager) StartStatefulShell() (int, error) {
	// This mimics: script -q -c "bash --norc --noprofile" /dev/null
	// The user should run this manually to get a shell, then get its PID
	return 0, fmt.Errorf("not implemented: users should manually start shell with 'script -q -c \"bash --norc --noprofile\" /dev/null'")
}
