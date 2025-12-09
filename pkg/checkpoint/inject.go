package checkpoint

// Command injection utilities for stateful shell sessions

import (
	"fmt"
	"os/exec"
	"path/filepath"
)

// InjectCommand injects a command into a running shell process using inject_and_capture.sh
// This preserves the stateful nature of the shell (environment variables, etc.)
// The inject_and_capture.sh script must be in the same directory as the checkpoint-lite binary
func (m *Manager) InjectCommand(pid int, command string) (string, error) {
	if !m.processExists(pid) {
		return "", fmt.Errorf("process %d does not exist", pid)
	}

	// Find inject_and_capture.sh script
	// Assume it's in the same directory as the checkpoint-lite binary
	scriptPath := "./inject_and_capture.sh"
	
	// Try absolute path if relative doesn't work
	if _, err := exec.LookPath(scriptPath); err != nil {
		// Try looking in common locations
		scriptPath = filepath.Join("/usr/local/bin", "inject_and_capture.sh")
		if _, err := exec.LookPath(scriptPath); err != nil {
			scriptPath = "/users/alexxjk/checkpoint-lite/inject_and_capture.sh"
		}
	}

	// Execute inject_and_capture.sh
	cmd := exec.Command(scriptPath, fmt.Sprintf("%d", pid), command)
	
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("command injection failed: %w", err)
	}

	return string(output), nil
}

// StartStatefulShell starts a new stateful bash shell using script command
// Returns the PID of the bash process that should be checkpointed
func (m *Manager) StartStatefulShell() (int, error) {
	// This mimics: script -q -c "bash --norc --noprofile" /dev/null
	// The user should run this manually to get a shell, then get its PID
	return 0, fmt.Errorf("not implemented: users should manually start shell with 'script -q -c \"bash --norc --noprofile\" /dev/null'")
}
