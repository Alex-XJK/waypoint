package checkpoint

// All CRIU-related operations

import (
	"bytes"
	"fmt"
	"os/exec"
	"syscall"
)

func (m *Manager) createMemoryCheckpoint(pid int, criuPath string) error {
	// Use CRIU to dump the process
	cmd := exec.Command("criu", "dump",
		"-t", fmt.Sprintf("%d", pid),
		"-D", criuPath,
		"-j",  // Leave the process running
		"--shell-job",
		"--file-locks",   // Include file locks
	)

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid: 0,
			Gid: 0,
		},
	}

	if err := cmd.Run(); err != nil {
		stderr := stderrBuf.String()
		fmt.Printf("CRIU stderr: %s\n", stderr)
		return fmt.Errorf("failed to create memory checkpoint: %w", err)
	}

	return nil
}

func (m *Manager) restoreMemoryState(pid int, criuPath string) (int, error) {
	// Kill the original process if it exists
	err := m.killProcess(pid)
	if err != nil {
		return -1, fmt.Errorf("failed to kill original process %d: %w", pid, err)
	}

	var cmd *exec.Cmd

	// Check if sandboxing is enabled
	if m.sandboxMode {
		cmd, err = RestoreInSandbox(criuPath, m.workOverlay, nil)
		if err != nil {
			return -1, fmt.Errorf("failed to setup sandbox for restore: %w", err)
		}
	} else {
		// Default behavior: no sandboxing
		criuCmd := fmt.Sprintf(
			"criu restore --images-dir '%s' -j --shell-job",
			criuPath,
		)

		cmd = exec.Command("script", "-q", "-c", criuCmd, "/dev/null")
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{
				Uid: 0,
				Gid: 0,
			},
		}
	}

	if err := cmd.Start(); err != nil {
		return -1, fmt.Errorf("failed to restore memory state: %w", err)
	}

	return pid, nil
}
