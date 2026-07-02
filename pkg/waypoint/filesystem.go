package waypoint

// All filesystem-related operations

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// InitEnvironment sets up OverlayFS for the given directory
func (m *Manager) InitEnvironment(originalDir string) (string, error) {
	// Convert to absolute path
	absDir, err := filepath.Abs(originalDir)
	if err != nil {
		return "", fmt.Errorf("failed to get absolute path: %w", err)
	}

	// Check if the user-specified directory exists
	if _, err := os.Stat(absDir); os.IsNotExist(err) {
		return "", fmt.Errorf("directory does not exist: %s", absDir)
	}

	m.originalDir = absDir

	// Create overlay structure
	upperDir := filepath.Join(m.baseDir, "current", "upper")
	workDir := filepath.Join(m.baseDir, "current", "work")

	os.MkdirAll(upperDir, 0755)
	os.MkdirAll(workDir, 0755)

	// Mount overlay
	err = m.mountOverlay([]string{absDir}, upperDir, workDir, m.workOverlay)
	if err != nil {
		return "", fmt.Errorf("failed to mount overlay: %w", err)
	}

	// Update session info with environment details
	if err := updateSessionEnvironment(m.sessionID, absDir, m.workOverlay); err != nil {
		return "", fmt.Errorf("failed to update session info: %w", err)
	}

	return m.workOverlay, nil
}

// mountOverlay mounts an OverlayFS filesystem
//
//	lowerDir: list of multiple lower directories
//	upperDir: upper directory
//	workDir: work directory
//	mountPoint: where to mount the overlay
func (m *Manager) mountOverlay(lowerDir []string, upperDir, workDir, mountPoint string) error {
	// Runtime pseudo filesystems are mounted under the merged mountpoint.
	// Tear them down before replacing the overlay mount.
	m.unmountRuntimeFS(mountPoint)

	// Unmount if already mounted
	exec.Command("umount", mountPoint).Run()

	// Mount overlay
	options := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", strings.Join(lowerDir, ":"), upperDir, workDir)
	cmd := exec.Command("mount", "-t", "overlay", "overlay", "-o", options, mountPoint)

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mount command failed: %w", err)
	}

	if err := m.mountRuntimeFS(mountPoint); err != nil {
		_ = exec.Command("umount", mountPoint).Run()
		return fmt.Errorf("mount runtime filesystems failed: %w", err)
	}

	return nil
}

func (m *Manager) mountRuntimeFS(mountPoint string) error {
	if strings.TrimSpace(mountPoint) == "" {
		return nil
	}
	type runtimeMount struct {
		relPath string
		fsType  string
		source  string
		data    string
	}
	mounts := []runtimeMount{
		{relPath: "proc", fsType: "proc", source: "proc"},
		{relPath: "sys", fsType: "sysfs", source: "sys"},
		// devpts is required by anything that opens a PTY via posix_openpt:
		// dpkg/apt's log writer, sudo, script, and interactive tools. Without it
		// they fail with "Is /dev/pts mounted?" (seen on qemu-startup /
		// qemu-alpine-ssh). newinstance keeps the pty set isolated per session.
		{relPath: "dev/pts", fsType: "devpts", source: "devpts", data: "newinstance,ptmxmode=0666,mode=0620"},
	}
	for _, rm := range mounts {
		target := filepath.Join(mountPoint, rm.relPath)
		if err := os.MkdirAll(target, 0o755); err != nil {
			return fmt.Errorf("mkdir runtime mount target %s failed: %w", target, err)
		}
		if err := unix.Mount(rm.source, target, rm.fsType, 0, rm.data); err != nil {
			if err == syscall.EBUSY {
				continue
			}
			return fmt.Errorf("mount %s on %s failed: %w", rm.fsType, target, err)
		}
	}
	// Point /dev/ptmx at the freshly-mounted devpts instance so posix_openpt
	// works (glibc opens /dev/ptmx). Best-effort: some images already ship one.
	ptmx := filepath.Join(mountPoint, "dev", "ptmx")
	if fi, err := os.Lstat(ptmx); err == nil && fi.Mode()&os.ModeSymlink == 0 {
		_ = os.Remove(ptmx)
	}
	_ = os.Symlink("pts/ptmx", ptmx)
	return nil
}

func (m *Manager) unmountRuntimeFS(mountPoint string) {
	if strings.TrimSpace(mountPoint) == "" {
		return
	}
	for _, rel := range []string{"dev/pts", "proc", "sys"} {
		target := filepath.Join(mountPoint, rel)
		if err := unix.Unmount(target, 0); err != nil {
			if err == syscall.EINVAL || err == syscall.ENOENT {
				continue
			}
			_ = unix.Unmount(target, unix.MNT_DETACH)
		}
	}
}

// forceUnmountOverlays unmounts all overlay filesystems in the session
func (m *Manager) forceUnmountOverlays() error {
	m.unmountRuntimeFS(m.workOverlay)

	// Unmount the main work overlay
	if m.workOverlay != "" {
		if err := m.forceUnmount(m.workOverlay); err != nil {
			return fmt.Errorf("failed to unmount work overlay: %w", err)
		}
	}

	// Find and unmount any other overlay mounts in our directory
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

// forceUnmount attempts to unmount with increasing force
func (m *Manager) forceUnmount(mountPoint string) error {
	fmt.Printf("Attempting to unmount [%s]...\n", mountPoint)
	// Try normal unmount first
	cmd := exec.Command("umount", mountPoint)
	if err := cmd.Run(); err == nil {
		return nil
	}

	// Try lazy unmount
	cmd = exec.Command("umount", "-l", mountPoint)
	if err := cmd.Run(); err == nil {
		return nil
	}

	// Try force unmount
	cmd = exec.Command("umount", "-f", mountPoint)
	return cmd.Run()
}

// findMountsInDirectory finds all mount points within our session directory
// Returns mounts sorted by depth (deepest first) for safe unmounting
func (m *Manager) findMountsInDirectory() ([]string, error) {
	// Use findmnt to find all mounts under baseDir
	// -r: raw output (no formatting)
	// -n: no headings
	// -o TARGET: output only the mount point
	// -M: find mounts under the specified mountpoint
	cmd := exec.Command("findmnt", "-r", "-n", "-o", "TARGET", "-M", m.baseDir)
	output, err := cmd.Output()
	if err != nil {
		// If findmnt fails, return empty slice (no mounts found)
		return []string{}, nil
	}

	// Parse output and filter mounts that start with baseDir
	var mounts []string
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && strings.HasPrefix(line, m.baseDir) {
			mounts = append(mounts, line)
		}
	}

	// Sort by depth (longest path = deepest mount, unmount first)
	for i := 0; i < len(mounts); i++ {
		for j := i + 1; j < len(mounts); j++ {
			if len(mounts[i]) < len(mounts[j]) {
				mounts[i], mounts[j] = mounts[j], mounts[i]
			}
		}
	}

	return mounts, nil
}

// forceUnmountAll uses umount to unmount everything in our directory tree
func (m *Manager) forceUnmountAll() error {
	// Find all mount points and force unmount them
	cmd := exec.Command("findmnt", "-n", "-o", "TARGET", "-M", m.baseDir)
	output, err := cmd.Output()
	if err != nil {
		return nil // No mounts found
	}

	mounts := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, mount := range mounts {
		if mount != "" {
			exec.Command("umount", "-f", "-l", mount).Run()
		}
	}

	return nil
}

// removeDirectoryWithRetry attempts to remove the base directory with exponential backoff
func (m *Manager) removeDirectoryWithRetry() error {
	maxAttempts := 5
	baseDelay := 500 * time.Millisecond

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err := os.RemoveAll(m.baseDir)
		if err == nil {
			return nil
		}

		if attempt == maxAttempts {
			return fmt.Errorf("final attempt failed: %w", err)
		}

		fmt.Printf("Attempt %d failed (%v), retrying in %v...\n",
			attempt, err, baseDelay)

		time.Sleep(baseDelay)
		baseDelay *= 2 // Exponential backoff
	}

	return nil
}

// buildOverlayLayers builds the list of overlay lower directories
// from the original directory and parent checkpoints' upper layers
// note: parentList is ordered from oldest to newest
// OverlayFS lowerdir priority: leftmost = highest priority
// So we want: [newest_ckpt, ..., oldest_ckpt, original]
func (m *Manager) buildOverlayLayers(parentList []string) []string {
	// Start with checkpoint layers in REVERSE order (newest first = highest priority)
	var lowerDirs []string
	for i := len(parentList) - 1; i >= 0; i-- {
		parentOverlay := filepath.Join(m.baseDir, parentList[i], "upper")
		lowerDirs = append(lowerDirs, parentOverlay)
	}
	// Original goes last (lowest priority)
	lowerDirs = append(lowerDirs, m.originalDir)
	return lowerDirs
}
