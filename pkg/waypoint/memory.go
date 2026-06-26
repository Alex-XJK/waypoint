package waypoint

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
	cmd := exec.Command("criu", "dump",
		"-t", fmt.Sprintf("%d", pid),
		"-D", criuPath,
		"--tcp-established",
		"--manage-cgroups=ignore",
		"--ghost-limit", "8388608",
		// Node 22 keeps inotify watches and unlinked-but-open files (e.g. the
		// bundled mock-api's working files). --force-irmap lets CRIU resolve
		// inotify watches via the inode reverse-map when the path is gone, and
		// --link-remap lets it dump deleted files that still have open fds.
		// Without both, dumping the shop process tree fails.
		"--force-irmap",
		"--link-remap",
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

func (m *Manager) restoreMemoryState(pid int, criuPath string) (int, error) {
	// Use CRIU to restore the process. --restore-detached makes CRIU exit
	// after a successful restore, so waypoint can report real failures.
	cmd := exec.Command(
		"criu", "restore",
		"--images-dir", criuPath,
		"--tcp-established",
		"--manage-cgroups=ignore",
		"--restore-detached",
		"-vv", "-o", "restore.log",
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
	devNull, _ := os.OpenFile("/dev/null", os.O_RDWR, 0)
	if devNull != nil {
		defer devNull.Close()
		cmd.Stdin = devNull
		cmd.Stdout = devNull
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Run(); err != nil {
		stderr := stderrBuf.String()
		if stderr != "" {
			fmt.Printf("CRIU stderr: %s\n", stderr)
		}
		return -1, fmt.Errorf("failed to restore memory state: %w", err)
	}

	return pid, nil
}
