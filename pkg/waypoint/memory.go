package waypoint

// All CRIU-related operations

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// tcpFlag decides what happens to established TCP connections. CRIU wants the
// same choice on both sides: dumping with --tcp-close and restoring without it
// fails with "Need to set the --tcp-close options."
//
// The default, --tcp-established, asks CRIU to carry the connection across the
// checkpoint and reconnect it on restore. That only works while the peer still
// has the connection open, so a checkpoint taken now and restored minutes later
// dies with "soccr: Can't connect inet socket back: Cannot assign requested
// address" -- and the whole restore fails with it, not just the socket.
//
// For a sandbox running network *clients* (an HTTP client with a keep-alive
// pool, say) --tcp-close is the sturdier choice: the connection is dropped at
// dump time and the client simply opens a new one after restore.
//
//	WAYPOINT_TCP=close   drop established connections at dump time
//	WAYPOINT_TCP=established (or unset)  current behaviour
func tcpFlag() string {
	if os.Getenv("WAYPOINT_TCP") == "close" {
		return "--tcp-close"
	}
	return "--tcp-established"
}

func (m *Manager) createMemoryCheckpoint(pid int, criuPath string) error {
	// Use CRIU to dump the process
	// Notice: Cannot use '--shell-job' because the PTY issue during the restore phase.
	cmd := exec.Command("criu", "dump",
		"-t", fmt.Sprintf("%d", pid),
		"-D", criuPath,
		tcpFlag(),
		"--manage-cgroups=ignore",
		"--force-irmap", // resolve inotify watches via the inode reverse-map when the path is gone
		"--link-remap",  // dump deleted files that still have open fds
		"--file-locks",
		"--ghost-limit", "8388608",
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
	// Use CRIU to restore the process.
	// Notice: Cannot use '--shell-job' because it will try to attach to the original PTY, which does not exist anymore.
	// --restore-detached makes CRIU exit after a successful restore, so waypoint can report real failures.
	cmd := exec.Command(
		"criu", "restore",
		"--images-dir", criuPath,
		tcpFlag(),
		"--manage-cgroups=ignore",
		"--restore-detached",
		"--file-locks",
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
