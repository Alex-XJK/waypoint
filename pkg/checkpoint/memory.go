package checkpoint

// All CRIU-related operations

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func (m *Manager) createMemoryCheckpoint(pid int, criuPath string) error {
	// Use CRIU to dump the process
	// Notice: Cannot use '--shell-job' because the PTY issue during the restore phase.
	cmd := exec.Command("criu-ns", "dump",
		"-t", fmt.Sprintf("%d", pid),
		"-D", criuPath,
		"-vv", "-o", "dump.log",
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

// for non shell processes
func (m *Manager) restoreMemoryState(pid int, criuPath string, pidFilename string) (int, error) {
	err := m.killProcess(pid)
	if err != nil {
		return -1, fmt.Errorf("failed to kill original process %d: %w", pid, err)
	}

	// Use CRIU to restore the process
	// Notice: Cannot use '--shell-job' because it will try to attach to the original PTY, which does not exist anymore.
	cmd := exec.Command(
		"criu-ns", "restore",
		"--restore-detached",
		"--images-dir", criuPath,
		"-vv", "-o", "restore.log", "--pidfile", pidFilename,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
	devNull, _ := os.OpenFile("/dev/null", os.O_RDWR, 0)
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull

	if err := cmd.Run(); err != nil { // do with --detached so that way returns after criu returns
		return -1, fmt.Errorf("failed to restore memory state: %w", err)
	}

	return pid, nil
}

func (m *Manager) createMemoryCheckpointShell(pid int, criuPath string) error {
	cmd := exec.Command("criu-ns", "dump",
		"-t", fmt.Sprintf("%d", pid),
		"-D", criuPath,
		"--external", fmt.Sprintf("tty[%x:%x]", m.shellRdev, m.shellDev),
		"-vv", "-o", "dump.log",
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
// for shell processes
func (m *Manager) restoreMemoryStateShell(pid int, criuPath string, pidFilename string, ckptDev, ckptRdev uint64) (int, error) {
	// Kill the original process if it exists
	err := m.killProcess(pid)
	if err != nil {
		return -1, fmt.Errorf("failed to kill original process %d: %w", pid, err)
	}

	minor := m.shellRdev & 0xff
	slavePath := fmt.Sprintf("/dev/pts/%d", minor)
	slave, _ := os.OpenFile(slavePath, os.O_RDWR|syscall.O_NOCTTY, 0)

	cmd := exec.Command(
		"criu-ns", "restore",
		//"-root", m.workOverlay, // 	DEBUG
		"--inherit-fd", fmt.Sprintf("fd[3]:tty[%x:%x]", ckptRdev, ckptDev),
		"--restore-detached",
		"--images-dir", criuPath,
		"-vv", "-o", "restore.log", "--pidfile", pidFilename,
	)
	cmd.ExtraFiles = []*os.File{slave}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
	devNull, _ := os.OpenFile("/dev/null", os.O_RDWR, 0)
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull

	if err := cmd.Run(); err != nil { // do with --detached so that way returns after criu returns
		return -1, fmt.Errorf("failed to restore memory state: %w", err)
	}

	return pid, nil
}
