package checkpoint

// Top-level checkpoint manager functions

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func NewManager(baseDir string) *Manager {
	metadataDir := filepath.Join(baseDir, "metadata")
	workOverlay := filepath.Join(baseDir, "work")
	temporaryDir := filepath.Join(baseDir, "temp")

	// Create directories
	os.MkdirAll(metadataDir, 0755)
	os.MkdirAll(workOverlay, 0755)
	os.MkdirAll(temporaryDir, 0777)

	return &Manager{
		baseDir:     baseDir,
		metadataDir: metadataDir,
		workOverlay: workOverlay,
	}
}

// ExecuteCommand executes a command in the checkpoint environment.
// If sandbox mode is enabled, the command runs in an isolated sandbox.
// Otherwise, it runs directly in the work overlay directory.
func (m *Manager) ExecuteCommand(command string, args ...string) (string, error) {
	if m.shellPid != ShellNotEnabled && m.shellSocket != "" {
		// If shell is enabled, execute command through the shell's sandbox
		socketPath := m.shellSocket
		commandString := command + " " + strings.Join(args, " ") + "\n"
		output, err := execCommand(socketPath, commandString)
		if err != nil {
			return "", fmt.Errorf("failed to execute command: %w", err)
		}
		return output, nil
	}

	// If shell is not enabled, execute command directly in the work overlay
	cmd := exec.Command(command, args...)
	cmd.Dir = m.workOverlay
	outputBytes, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to execute command: %w\nOutput: %s", err, string(outputBytes))
	}
	return string(outputBytes), nil
}

// CreateCheckpoint creates both the filesystem and the memory checkpoint
// Deprecated: Since version 0.2.0, use CreateCheckpointParallel instead

// CreateCheckpointParallel creates both checkpoints in parallel, speeding up the process (approx x0.65)
// Deprecated: Since version 0.4.0, use CreateCheckpointNew instead

// CreateCheckpointNew creates a new checkpoint with the given ID
func (m *Manager) CreateCheckpointNew(pid int, checkpointID string) error {
	// Validate checkpoint ID
	// TODO: Check for duplication
	if checkpointID == "" || checkpointID == "current" {
		return fmt.Errorf("invalid checkpoint ID: %s", checkpointID)
	}

	// Special case for Default PID: Use Bash PID if available, otherwise skip memory checkpoint
	if pid == PidNotProvided {
		if m.shellPid != ShellNotEnabled {
			pid = m.shellPid
		} else {
			pid = SkipMemoryCheckpoint
		}
	}

	// Create a memory checkpoint to "~/current/criu/*.img"
	if pid == SkipMemoryCheckpoint {
		fmt.Println("Skipping memory checkpoint as per user request")
	} else if !m.processExists(pid) {
		return fmt.Errorf("process %d does not exist", pid)
	} else {
		currentCriuDir := filepath.Join(m.currDir, "criu")
		os.RemoveAll(currentCriuDir)
		os.MkdirAll(currentCriuDir, 0755)
		memoryErr := m.createMemoryCheckpoint(pid, currentCriuDir)
		if memoryErr != nil {
			return fmt.Errorf("memory checkpoint failed: %w", memoryErr)
		}
	}

	// Unmount current overlay to ensure filesystem consistency
	exec.Command("umount", m.workOverlay).Run()

	// Rename "~/current/" to "~/<checkpointID>/"
	// currentDir := filepath.Join(m.baseDir, "current")
	ckptDir := filepath.Join(m.baseDir, checkpointID)
	if err := os.Rename(m.currDir, ckptDir); err != nil {
		return fmt.Errorf("failed to rename current directory: %w", err)
	}

	// Recreate a new empty "current" overlay for continued use
	os.MkdirAll(m.currDir, 0755)
	upperDir := filepath.Join(m.currDir, "upper")
	workDir := filepath.Join(m.currDir, "work")
	os.MkdirAll(upperDir, 0755)
	os.MkdirAll(workDir, 0755)

	// Update current parent list to include this new checkpoint
	parentList := m.currentParent
	parentList = append(parentList, checkpointID)
	m.currentParent = parentList
	m.syncManagerToSession()

	// Remount the new "current" overlay with multiple lowerdirs
	lowerDirs := m.buildOverlayLayers(parentList)
	err := m.mountOverlay(lowerDirs, upperDir, workDir, m.workOverlay)
	if err != nil {
		return fmt.Errorf("failed to remount new current overlay: %w", err)
	}

	// Restore the memory state into the new overlay
	// (so that the process can continue running in the new overlay)
	if pid != SkipMemoryCheckpoint {
		currentCriuDir := filepath.Join(m.baseDir, checkpointID, "criu")
		newPID, errMem := m.restoreMemoryState(pid, currentCriuDir)
		if errMem != nil {
			return fmt.Errorf("memory restore into new overlay failed: %w", errMem)
		}
		fmt.Printf("Process %d restored into new overlay with PID %d\n", pid, newPID)
	}

	// Save metadata
	metadata := Metadata{
		ID:          checkpointID,
		PID:         pid,
		Timestamp:   time.Now().Unix(),
		OriginalDir: m.originalDir,
		SessionID:   m.sessionID,
		ParentList:  parentList,
	}

	return m.saveMetadata(checkpointID, metadata)
}

func (m *Manager) RestoreCheckpointNew(checkpointID string) (int, error) {
	// Load checkpointMetadata
	checkpointMetadata, err := m.loadMetadata(checkpointID)
	if err != nil {
		return 0, fmt.Errorf("failed to load checkpoint metadata: %w", err)
	}

	// If the previous checkpoint contains process, we need to first kill it, so that mountpoint can be released.
	if checkpointMetadata.PID != SkipMemoryCheckpoint {
		if err := m.killProcess(checkpointMetadata.PID); err != nil {
			return 0, fmt.Errorf("failed to kill original process %d: %w", checkpointMetadata.PID, err)
		}
	}

	// Unmount current overlay for future remount
	exec.Command("umount", m.workOverlay).Run()

	// Clear current upper and work directories
	upperDir := filepath.Join(m.currDir, "upper")
	workDir := filepath.Join(m.currDir, "work")
	os.RemoveAll(upperDir)
	os.RemoveAll(workDir)

	// Rebuild lowerdirs list from checkpointMetadata.ParentList
	lowerDirs := m.buildOverlayLayers(checkpointMetadata.ParentList)

	// Update current parent list to checkpoint's parent list
	m.currentParent = checkpointMetadata.ParentList
	m.syncManagerToSession()

	// Remount overlay with the checkpoint's upper layer on top of the parent lowerdirs
	os.MkdirAll(upperDir, 0755)
	os.MkdirAll(workDir, 0755)
	errFs := m.mountOverlay(lowerDirs, upperDir, workDir, m.workOverlay)
	if errFs != nil {
		return 0, fmt.Errorf("filesystem restore failed: %w", errFs)
	}

	// Restore memory state using CRIU
	if checkpointMetadata.PID == SkipMemoryCheckpoint {
		fmt.Println("Skipping memory restore as per user request")
		return SkipMemoryCheckpoint, nil
	}
	previousCriuPath := filepath.Join(m.baseDir, checkpointID, "criu")
	newPID, errMem := m.restoreMemoryState(checkpointMetadata.PID, previousCriuPath)
	if errMem != nil {
		return 0, fmt.Errorf("memory restore failed: %w", errMem)
	}

	return newPID, nil
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

	// Unmount overlay
	if m.workOverlay != "" {
		cmd := exec.Command("umount", m.workOverlay)
		cmd.Run() // Ignore errors - might already be unmounted
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
	fmt.Printf("Starting forceful cleanup for session %s...\n", m.sessionID)

	// Step 1: Kill processes using files in this directory
	fmt.Println("Killing processes using session directory...")
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
