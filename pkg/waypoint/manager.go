package waypoint

// Top-level checkpoint manager functions

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func NewManager(baseDir string) *Manager {
	metadataDir := filepath.Join(baseDir, "metadata")
	workOverlay := filepath.Join(baseDir, "work")
	temporaryDir := filepath.Join(baseDir, "temp")

	// Create directories
	os.MkdirAll(metadataDir, 0755)
	os.MkdirAll(workOverlay, 0755)
	os.MkdirAll(temporaryDir, 0777)
	os.MkdirAll(filepath.Join(baseDir, "checkpoints"), 0755)
	os.MkdirAll(filepath.Join(baseDir, "forks"), 0755)
	os.MkdirAll(filepath.Join(baseDir, "locks"), 0755)

	return &Manager{
		baseDir:     baseDir,
		metadataDir: metadataDir,
		workOverlay: workOverlay,
	}
}

// ListCheckpoints returns a list of available checkpoints
func (m *Manager) ListCheckpoints() ([]string, error) {
	files, err := os.ReadDir(m.metadataDir)
	if err != nil {
		return nil, err
	}

	var checkpoints []string
	for _, file := range files {
		if strings.HasSuffix(file.Name(), ".json") && file.Name() != "environment.json" {
			checkpointID := strings.TrimSuffix(file.Name(), ".json")
			checkpoints = append(checkpoints, checkpointID)
		}
	}

	return checkpoints, nil
}

// Cleanup removes all files and unmounts the overlay for this session
func (m *Manager) Cleanup() error {
	loadConfig()

	// Cleanup shell related resources if shell enabled
	if m.shellPid != ShellNotEnabled {
		if err := m.killProcess(m.shellPid); err != nil {
			fmt.Printf("Warning: Failed to kill shell process: %v\n", err)
		} else {
			// Remove the socket file if it exists
			if m.shellSocket != "" {
				// Ignore errors - might already be removed
				os.Remove(m.shellSocket)
			}
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
		m.unmountRuntimeFS(m.workOverlay)
		cmd := exec.Command("umount", m.workOverlay)
		cmd.Run() // Ignore errors - might already be unmounted
	}

	if PreserveSessionOnCleanup {
		fmt.Printf("Preserving session directory and session info for %s\n", m.sessionID)
		return nil
	}

	// Remove session directory
	if err := os.RemoveAll(m.baseDir); err != nil {
		return fmt.Errorf("failed to remove session directory: %w", err)
	}

	// Remove global session info
	return removeSessionInfo(m.sessionID)
}

// CleanupForce removes all files and unmounts the overlay for this session
func (m *Manager) CleanupForce() error {
	loadConfig()

	fmt.Printf("Starting forceful cleanup for session %s...\n", m.sessionID)

	// Step 1: Kill processes using files in this directory
	fmt.Println("Killing processes using session directory...")
	if forks, err := m.ListForks(); err == nil {
		for _, f := range forks {
			if f.PID > 0 {
				_ = m.killProcess(f.PID)
			}
		}
	}
	if err := m.killProcessesUsingDirectory(); err != nil {
		fmt.Printf("Warning: Failed to kill some processes: %v\n", err)
	}

	// Step 2: Close file handles
	fmt.Println("Closing file handles...")
	if err := m.closeFileHandles(); err != nil {
		fmt.Printf("Warning: Failed to close some file handles: %v\n", err)
	}

	// Step 3: Unmount overlay filesystems
	fmt.Println("Unmounting overlay filesystems...")
	if err := m.forceUnmountOverlays(); err != nil {
		fmt.Printf("Warning: Failed to unmount overlays: %v\n", err)
	}

	// Step 4: Force unmount any remaining mounts
	fmt.Println("Force unmounting all mounts in session directory...")
	if err := m.forceUnmountAll(); err != nil {
		fmt.Printf("Warning: Failed to force unmount: %v\n", err)
	}

	if PreserveSessionOnCleanup {
		fmt.Printf("Preserving session directory and session info for %s\n", m.sessionID)
		return nil
	}

	// Step 5: Try removing the directory multiple times with a backoff
	fmt.Println("Removing session directory...")
	if err := m.removeDirectoryWithRetry(); err != nil {
		// Must error out if we cannot remove the directory, otherwise we might leave a broken session
		return fmt.Errorf("failed to remove session directory after multiple attempts: %w", err)
	}

	// Step 6: Remove global session info
	fmt.Println("Removing session info...")
	if err := removeSessionInfo(m.sessionID); err != nil {
		fmt.Printf("Warning: Failed to remove session info: %v\n", err)
	}

	return nil
}

// CleanupInteractive cleanup with user interaction
func (m *Manager) CleanupInteractive() error {
	// Try automatic cleanup first
	err := m.Cleanup()
	if err == nil {
		return nil
	}

	fmt.Printf("Automatic cleanup failed: %v\n", err)
	fmt.Println("This usually happens when processes are still using files in the session directory.")
	fmt.Println("\nTroubleshooting hints:")

	// Show processes using the directory
	fmt.Printf("Processes using session directory:\n")
	pids, _ := m.findProcessesUsingDirectory()
	if len(pids) > 0 {
		for _, pid := range pids {
			cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "pid,ppid,cmd")
			output, _ := cmd.Output()
			fmt.Print(string(output))
		}
	} else {
		fmt.Print("  No processes found")
	}
	fmt.Println()

	// Show mount points
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
