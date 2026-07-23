package waypoint

// Session teardown: the graceful, forced, and interactive cleanup flows and
// their supporting process-kill and force-unmount machinery
// (lsof/fuser/findmnt-based).

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Cleanup removes all files and unmounts the overlay for this session.
func (m *Manager) Cleanup() error {
	loadConfig()

	// Cleanup shell related resources if shell enabled
	if m.shellPid != ShellNotEnabled {
		if err := killProcess(m.shellPid); err != nil {
			fmt.Printf("Warning: Failed to kill shell process: %v\n", err)
		} else if m.shellSocket != "" {
			// Ignore errors - might already be removed
			os.Remove(m.shellSocket)
		}
	}
	if forks, err := m.ListForks(); err == nil {
		for _, f := range forks {
			if err := m.DestroyFork(f.ID); err != nil {
				fmt.Printf("Warning: Failed to destroy fork %s: %v\n", f.ID, err)
			}
		}
	}

	// Unmount overlay
	if m.workOverlay != "" {
		unmountRuntimeFS(m.workOverlay)
		_ = unix.Unmount(m.workOverlay, 0) // might already be unmounted
	}

	m.removeTmpfsImages()

	if PreserveSessionOnCleanup {
		fmt.Printf("Preserving session directory and session info for %s\n", m.sessionID)
		return nil
	}

	if err := os.RemoveAll(m.baseDir); err != nil {
		return fmt.Errorf("failed to remove session directory: %w", err)
	}
	return removeSessionInfo(m.sessionID)
}

// CleanupForce tears the session down even when processes or mounts are
// still holding it: kill holders, close handles, force-unmount, then remove.
func (m *Manager) CleanupForce() error {
	loadConfig()

	fmt.Printf("Starting forceful cleanup for session %s...\n", m.sessionID)

	fmt.Println("Killing processes using session directory...")
	if forks, err := m.ListForks(); err == nil {
		for _, f := range forks {
			if f.PID > 0 {
				_ = killProcess(f.PID)
			}
		}
	}
	if err := m.killProcessesUsingDirectory(); err != nil {
		fmt.Printf("Warning: Failed to kill some processes: %v\n", err)
	}

	fmt.Println("Closing file handles...")
	if err := m.closeFileHandles(); err != nil {
		fmt.Printf("Warning: Failed to close some file handles: %v\n", err)
	}

	fmt.Println("Unmounting overlay filesystems...")
	if err := m.forceUnmountOverlays(); err != nil {
		fmt.Printf("Warning: Failed to unmount overlays: %v\n", err)
	}

	fmt.Println("Force unmounting all mounts in session directory...")
	if err := m.forceUnmountAll(); err != nil {
		fmt.Printf("Warning: Failed to force unmount: %v\n", err)
	}

	m.removeTmpfsImages()

	if PreserveSessionOnCleanup {
		fmt.Printf("Preserving session directory and session info for %s\n", m.sessionID)
		return nil
	}

	fmt.Println("Removing session directory...")
	if err := m.removeDirectoryWithRetry(); err != nil {
		// Must error out if we cannot remove the directory, otherwise we might leave a broken session
		return fmt.Errorf("failed to remove session directory after multiple attempts: %w", err)
	}

	fmt.Println("Removing session info...")
	if err := removeSessionInfo(m.sessionID); err != nil {
		fmt.Printf("Warning: Failed to remove session info: %v\n", err)
	}

	return nil
}

// removeTmpfsImages drops this session's tmpfs image dirs (checkpoints not
// yet — or never — flushed to disk). Best-effort: the dir may not exist.
func (m *Manager) removeTmpfsImages() {
	if m.sessionID == "" {
		return
	}
	if err := os.RemoveAll(m.sessionTmpfsDir()); err != nil {
		fmt.Printf("Warning: failed to remove tmpfs images dir: %v\n", err)
	}
}

// CleanupInteractive tries the graceful cleanup, then prints troubleshooting
// hints (holding processes, live mounts) if it fails.
func (m *Manager) CleanupInteractive() error {
	err := m.Cleanup()
	if err == nil {
		return nil
	}

	fmt.Printf("Automatic cleanup failed: %v\n", err)
	fmt.Println("This usually happens when processes are still using files in the session directory.")
	fmt.Println("\nTroubleshooting hints:")

	fmt.Printf("Processes using session directory:\n")
	pids, _ := m.findProcessesUsingDirectory()
	if len(pids) > 0 {
		for _, pid := range pids {
			output, _ := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "pid,ppid,cmd").Output()
			fmt.Print(string(output))
		}
	} else {
		fmt.Print("  No processes found")
	}
	fmt.Println()

	fmt.Printf("Active mount points:\n")
	mounts, _ := m.findMountsInDirectory()
	if len(mounts) > 0 {
		for _, mount := range mounts {
			fmt.Printf("  %s\n", mount)
		}
	} else {
		fmt.Print("  No mounts found")
	}
	fmt.Println()

	fmt.Println("\nRecommended actions:")
	fmt.Println("1. Close any terminals/editors in the session directory")
	fmt.Println("2. Deactivate Python virtual environments, Docker containers, etc.")
	fmt.Println("3. Stop any processes listed above")
	fmt.Println("4. Unmount any mounts listed above (e.g., using 'sudo umount <mountpoint>')")

	return fmt.Errorf("manual intervention required")
}

// --- process helpers ---

func processExists(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 only checks that the process exists
	return process.Signal(syscall.Signal(0)) == nil
}

// killProcess terminates a process gracefully (SIGTERM), escalating to
// SIGKILL if it does not exit within the grace period.
func killProcess(pid int) error {
	if !processExists(pid) {
		return nil
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("failed to retrieve process %d: %w", pid, err)
	}

	if err := process.Signal(syscall.SIGTERM); err != nil {
		if err := process.Signal(syscall.SIGKILL); err != nil {
			return fmt.Errorf("failed to kill process %d: %w", pid, err)
		}
	}

	// Wait for the process to terminate (up to 5 seconds); CRIU needs the
	// checkpointed task to disappear before its resources can be reused.
	for i := 0; i < 50; i++ {
		if !processExists(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Still running after the grace period; force kill
	return process.Signal(syscall.SIGKILL)
}

// killProcessesUsingDirectory kills processes that have files open in our directory
func (m *Manager) killProcessesUsingDirectory() error {
	pids, err := m.findProcessesUsingDirectory()
	if err != nil {
		return err
	}
	if len(pids) == 0 {
		return nil
	}

	fmt.Printf("Found %d processes using directory, attempting to terminate...\n", len(pids))

	errorChan := make(chan error, len(pids))
	for _, pid := range pids {
		go func(pid int) {
			if err := killProcess(pid); err != nil {
				errorChan <- fmt.Errorf("failed to kill process %d: %w", pid, err)
			} else {
				fmt.Printf("Successfully killed process %d\n", pid)
				errorChan <- nil
			}
		}(pid)
	}

	failed := 0
	for range pids {
		if err := <-errorChan; err != nil {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("failed to kill %d out of %d processes using directory", failed, len(pids))
	}
	return nil
}

// findProcessesUsingDirectory uses lsof to find processes with open files in directory
func (m *Manager) findProcessesUsingDirectory() ([]int, error) {
	output, err := exec.Command("lsof", "+D", m.baseDir).Output()
	if err != nil {
		// lsof returns a non-zero exit code if no files are found, which is not an error
		return []int{}, nil
	}

	var pids []int
	seen := map[int]bool{}
	lines := strings.Split(string(output), "\n")

	// Skip header line, parse PIDs from lsof output
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if pid, err := strconv.Atoi(fields[1]); err == nil && !seen[pid] {
			seen[pid] = true
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// closeFileHandles attempts to close file handles using fuser
func (m *Manager) closeFileHandles() error {
	exec.Command("fuser", "-k", m.baseDir).Run()
	return nil
}

// --- force-unmount machinery ---

// forceUnmountOverlays unmounts all overlay filesystems in the session
func (m *Manager) forceUnmountOverlays() error {
	unmountRuntimeFS(m.workOverlay)

	if m.workOverlay != "" {
		if err := m.forceUnmount(m.workOverlay); err != nil {
			return fmt.Errorf("failed to unmount work overlay: %w", err)
		}
	}

	mounts, err := m.findMountsInDirectory()
	if err != nil {
		return err
	}
	for _, mount := range mounts {
		if err := m.forceUnmount(mount); err != nil {
			fmt.Printf("Warning: Failed to unmount %s: %v\n", mount, err)
		}
	}
	return nil
}

// forceUnmount attempts to unmount with increasing force: normal, lazy, forced.
func (m *Manager) forceUnmount(mountPoint string) error {
	fmt.Printf("Attempting to unmount [%s]...\n", mountPoint)
	if err := exec.Command("umount", mountPoint).Run(); err == nil {
		return nil
	}
	if err := exec.Command("umount", "-l", mountPoint).Run(); err == nil {
		return nil
	}
	return exec.Command("umount", "-f", mountPoint).Run()
}

// findMountsInDirectory finds all mount points within our session directory,
// sorted by depth (deepest first) for safe unmounting.
func (m *Manager) findMountsInDirectory() ([]string, error) {
	// -r raw output, -n no headings, -o TARGET mount point only,
	// -M find mounts under the given mountpoint
	output, err := exec.Command("findmnt", "-r", "-n", "-o", "TARGET", "-M", m.baseDir).Output()
	if err != nil {
		// If findmnt fails, treat it as no mounts found
		return []string{}, nil
	}

	var mounts []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && strings.HasPrefix(line, m.baseDir) {
			mounts = append(mounts, line)
		}
	}

	// Longest path = deepest mount, unmount first
	sort.Slice(mounts, func(i, j int) bool { return len(mounts[i]) > len(mounts[j]) })
	return mounts, nil
}

// forceUnmountAll lazily force-unmounts everything in our directory tree
func (m *Manager) forceUnmountAll() error {
	output, err := exec.Command("findmnt", "-n", "-o", "TARGET", "-M", m.baseDir).Output()
	if err != nil {
		return nil // No mounts found
	}

	for _, mount := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if mount != "" {
			exec.Command("umount", "-f", "-l", mount).Run()
		}
	}
	return nil
}

// removeDirectoryWithRetry attempts to remove the base directory with exponential backoff
func (m *Manager) removeDirectoryWithRetry() error {
	maxAttempts := 5
	delay := 500 * time.Millisecond

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err := os.RemoveAll(m.baseDir)
		if err == nil {
			return nil
		}
		if attempt == maxAttempts {
			return fmt.Errorf("final attempt failed: %w", err)
		}

		fmt.Printf("Attempt %d failed (%v), retrying in %v...\n", attempt, err, delay)
		time.Sleep(delay)
		delay *= 2
	}
	return nil
}
