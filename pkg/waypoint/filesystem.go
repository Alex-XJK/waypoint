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

	// Unmount if already mounted. This must not be best-effort: mounting on top
	// of a mountpoint that failed to unmount stacks a second overlay on the same
	// path, and every later teardown only ever peels off the topmost one.
	if err := m.unmountAll(mountPoint); err != nil {
		return fmt.Errorf("failed to unmount existing overlay at %s: %w", mountPoint, err)
	}

	// Mount overlay
	options := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", strings.Join(lowerDir, ":"), upperDir, workDir)
	cmd := exec.Command("mount", "-t", "overlay", "overlay", "-o", options, mountPoint)

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mount command failed: %w", err)
	}

	if err := m.mountRuntimeFS(mountPoint); err != nil {
		_ = m.unmountAll(mountPoint)
		return fmt.Errorf("mount runtime filesystems failed: %w", err)
	}

	return nil
}

// mountPointCount reports how many times path appears as a mount target in the
// current mount namespace. Stacked mounts on the same directory each count once,
// which is what makes it possible to tell "unmounted" from "one layer down".
func mountPointCount(path string) (int, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return 0, fmt.Errorf("failed to read mountinfo: %w", err)
	}

	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		// Field 5 (1-indexed) of a mountinfo line is the mount point.
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if unescapeMountField(fields[4]) == path {
			count++
		}
	}
	return count, nil
}

// unescapeMountField decodes the octal escapes the kernel writes into mountinfo
// paths for characters that would otherwise break the field split.
func unescapeMountField(field string) string {
	replacer := strings.NewReplacer(
		`\040`, " ",
		`\011`, "\t",
		`\012`, "\n",
		`\134`, `\`,
	)
	return replacer.Replace(field)
}

// isMountPoint reports whether path currently has anything mounted on it.
func isMountPoint(path string) bool {
	count, err := mountPointCount(path)
	return err == nil && count > 0
}

// unmountAll unmounts every mount stacked on mountPoint, topmost first, until the
// path is no longer a mount target. Unlike a single `umount`, it converges when
// overlays have been stacked by earlier failed teardowns.
func (m *Manager) unmountAll(mountPoint string) error {
	if strings.TrimSpace(mountPoint) == "" {
		return nil
	}

	for i := 0; i < maxMountLayers; i++ {
		count, err := mountPointCount(mountPoint)
		if err != nil {
			return err
		}
		if count == 0 {
			return nil
		}
		if err := unix.Unmount(mountPoint, 0); err != nil {
			// EINVAL means it is not a mountpoint after all, so we are done.
			if err == syscall.EINVAL {
				return nil
			}
			return fmt.Errorf("unmount %s failed: %w", mountPoint, err)
		}
	}

	return fmt.Errorf("unmount %s: still mounted after %d layers", mountPoint, maxMountLayers)
}

// waitForMountCountBelow waits until mountPoint appears fewer than target times in
// the mount table. Lazy unmounts detach asynchronously, so the mount entry does
// not disappear the moment the call returns.
func waitForMountCountBelow(mountPoint string, target int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		count, err := mountPointCount(mountPoint)
		if err != nil {
			return err
		}
		if count < target {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("lazy unmount of %s did not settle within %s", mountPoint, timeout)
		}
		time.Sleep(processPollInterval)
	}
}

func (m *Manager) mountRuntimeFS(mountPoint string) error {
	if strings.TrimSpace(mountPoint) == "" {
		return nil
	}
	type runtimeMount struct {
		relPath string
		fsType  string
		source  string
	}
	mounts := []runtimeMount{
		{relPath: "proc", fsType: "proc", source: "proc"},
		{relPath: "sys", fsType: "sysfs", source: "sys"},
	}
	for _, rm := range mounts {
		target := filepath.Join(mountPoint, rm.relPath)
		if err := os.MkdirAll(target, 0o755); err != nil {
			return fmt.Errorf("mkdir runtime mount target %s failed: %w", target, err)
		}
		if err := unix.Mount(rm.source, target, rm.fsType, 0, ""); err != nil {
			if err == syscall.EBUSY {
				continue
			}
			return fmt.Errorf("mount %s on %s failed: %w", rm.fsType, target, err)
		}
	}
	return nil
}

func (m *Manager) unmountRuntimeFS(mountPoint string) {
	if strings.TrimSpace(mountPoint) == "" {
		return
	}
	for _, rel := range []string{"proc", "sys"} {
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

// forceUnmount attempts to unmount with increasing force, peeling off every
// layer stacked on mountPoint rather than just the topmost one.
func (m *Manager) forceUnmount(mountPoint string) error {
	if strings.TrimSpace(mountPoint) == "" {
		return nil
	}
	fmt.Printf("Attempting to unmount [%s]...\n", mountPoint)

	for i := 0; i < maxMountLayers; i++ {
		count, err := mountPointCount(mountPoint)
		if err != nil {
			return err
		}
		if count == 0 {
			return nil
		}
		if err := m.forceUnmountOneLayer(mountPoint, count); err != nil {
			return err
		}
	}

	return fmt.Errorf("unmount %s: still mounted after %d layers", mountPoint, maxMountLayers)
}

// forceUnmountOneLayer removes a single mount layer, escalating from a plain
// unmount to a forced one and finally to a lazy detach. `count` is the number of
// layers observed before the attempt, used to confirm a lazy detach completed.
func (m *Manager) forceUnmountOneLayer(mountPoint string, count int) error {
	// Try normal unmount first
	if err := unix.Unmount(mountPoint, 0); err == nil {
		return nil
	}

	// Try force unmount (mainly helps unresponsive network filesystems)
	if err := exec.Command("umount", "-f", mountPoint).Run(); err == nil {
		return nil
	}

	// Lazy unmount as the last resort. It detaches asynchronously, so waiting is
	// what keeps callers from deleting the directory out from under a mount that
	// has not actually gone away yet.
	if err := unix.Unmount(mountPoint, unix.MNT_DETACH); err != nil {
		return fmt.Errorf("lazy unmount %s failed: %w", mountPoint, err)
	}
	return waitForMountCountBelow(mountPoint, count, unmountSettleTimeout)
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
