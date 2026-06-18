package waypoint

// All CRIU-related operations

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

func (m *Manager) createMemoryCheckpoint(pid int, criuPath string) error {
	// Use CRIU to dump the process
	// Notice: Cannot use '--shell-job' because the PTY issue during the restore phase.
	args := []string{"dump",
		"-t", fmt.Sprintf("%d", pid),
		"-D", criuPath,
		"--tcp-established",
		"--ghost-limit", "8388608",
		"-vv", "-o", "dump.log",
	}
	if mountID, err := findMountID(pid, m.workOverlay); err == nil && mountID != "" {
		args = append(args, "--external", fmt.Sprintf("mnt[%s]:waypoint-work", mountID))
	}

	cmd := exec.Command("criu", args...)

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

func findMountID(pid int, mountPoint string) (string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", fmt.Sprintf("%d", pid), "mountinfo"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if unescapeMountInfoPath(fields[4]) == mountPoint {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("mount %s not found in pid %d mountinfo", mountPoint, pid)
}

func unescapeMountInfoPath(path string) string {
	replacer := strings.NewReplacer(
		`\\`, `\`,
		`\040`, " ",
		`\011`, "\t",
		`\012`, "\n",
	)
	return replacer.Replace(path)
}

func (m *Manager) restoreMemoryState(pid int, criuPath string) (int, error) {
	// Use CRIU to restore the process
	// Notice: Cannot use '--shell-job' because it will try to attach to the original PTY, which does not exist anymore.
	cmd := exec.Command(
		"criu", "restore",
		"--images-dir", criuPath,
		"--tcp-established",
		"-vv", "-o", "restore.log",
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
	devNull, _ := os.OpenFile("/dev/null", os.O_RDWR, 0)
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull

	if err := cmd.Start(); err != nil {
		return -1, fmt.Errorf("failed to restore memory state: %w", err)
	}

	return pid, nil
}
