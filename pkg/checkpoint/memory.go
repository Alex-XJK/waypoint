package checkpoint

// All CRIU-related operations

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

func (m *Manager) createMemoryCheckpoint(pid int, criuPath string) error {
	// Use CRIU to dump the process
	// Notice: Cannot use '--shell-job' because the PTY issue during the restore phase.
	useInc, prevRel := m.isIncrementalPossible()

	var cmd *exec.Cmd
	if useInc {
		// Use the Incremental Dump
		// criu dump --tree <pid> --images-dir <B> --prev-images-dir <A-relative-to-B> --track-mem
		cmd = exec.Command(
			"criu", "dump",
			"-t", fmt.Sprintf("%d", pid),
			"-D", criuPath,
			"--prev-images-dir", prevRel,
			"--track-mem",
			"-vv", "-o", "dump.log",
		)
	} else {
		// Use the original criu dump
		cmd = exec.Command(
			"criu", "dump",
			"-t", fmt.Sprintf("%d", pid),
			"-D", criuPath,
			"--track-mem",
			"-vv", "-o", "dump.log",
		)
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	cmd.Dir = criuPath
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

	// Use CRIU to restore the process
	// Notice: Cannot use '--shell-job' because it will try to attach to the original PTY, which does not exist anymore.
	cmd := exec.Command(
		"criu", "restore",
		"--images-dir", criuPath,
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

// isIncrementalPossible Checks whether there is a pre-dump image exist
// if found, return the path to that directory relative to this one
func (m *Manager) isIncrementalPossible() (bool, string) {
	parLength := len(m.currentParent)
	if parLength > 0 {
		// Get the latest parent snapshot name
		lastParent := m.currentParent[parLength-1]
		prevDir := filepath.Join(m.baseDir, lastParent, "criu")
		if st, err := os.Stat(prevDir); err == nil && st.IsDir() {
			// Compute path to previous images relative to current images dir
			curDir := filepath.Join(m.baseDir, "current", "criu")
			if rel, err := filepath.Rel(curDir, prevDir); err == nil && rel != "" {
				return true, rel
			}
			// Fallback to a conservative relative path
			return true, filepath.Join("..", "..", lastParent, "criu")
		}
	} 
	return false, ""
}
