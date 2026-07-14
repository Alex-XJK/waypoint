package waypoint

// OverlayFS assembly: mounting the merged view of checkpoint layers plus the
// runtime pseudo-filesystems (/proc, /sys) beneath it. Used both on the host
// (initial mount for `main`) and inside a fork restore child's fresh mount
// namespace.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// InitEnvironment sets up the first OverlayFS mount for the given directory.
func (m *Manager) InitEnvironment(originalDir string) (string, error) {
	absDir, err := filepath.Abs(originalDir)
	if err != nil {
		return "", fmt.Errorf("failed to get absolute path: %w", err)
	}
	if _, err := os.Stat(absDir); os.IsNotExist(err) {
		return "", fmt.Errorf("directory does not exist: %s", absDir)
	}

	m.originalDir = absDir

	// The `main` fork owns the initial overlay's private CoW layers.
	mainRoot := m.forkDir(MainForkID)
	upperDir := filepath.Join(mainRoot, "upper")
	workDir := filepath.Join(mainRoot, "work")
	for _, dir := range []string{upperDir, workDir, filepath.Join(mainRoot, "temp")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}

	if err := mountOverlay([]string{absDir}, upperDir, workDir, m.workOverlay); err != nil {
		return "", fmt.Errorf("failed to mount overlay: %w", err)
	}

	if err := updateSessionEnvironment(m.sessionID, absDir, m.workOverlay); err != nil {
		return "", fmt.Errorf("failed to update session info: %w", err)
	}

	return m.workOverlay, nil
}

// prepareForkMountNamespace mounts a fork's private overlay view at the
// session's canonical mountpoint. Runs inside the restore child's fresh
// mount namespace, so it first stops mounts from propagating back out.
func prepareForkMountNamespace(f *Fork) error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mount namespace private failed: %w", err)
	}

	sessionDir := filepath.Dir(filepath.Dir(f.RootDir))
	lowerDirs := overlayLowerDirs(filepath.Join(sessionDir, "checkpoints"), f.LayerIDs, f.OriginalDir)
	return mountOverlay(lowerDirs, f.UpperDir, f.WorkDir, f.MountPoint)
}

// overlayLowerDirs turns a checkpoint layer chain into ordered overlay
// lowerdirs. layerIDs is ordered oldest to newest; OverlayFS gives the
// leftmost lowerdir the highest priority, so layers are emitted newest first
// with the original rootfs last.
func overlayLowerDirs(checkpointsDir string, layerIDs []string, originalDir string) []string {
	dirs := make([]string, 0, len(layerIDs)+1)
	for i := len(layerIDs) - 1; i >= 0; i-- {
		dirs = append(dirs, filepath.Join(checkpointsDir, layerIDs[i], "upper"))
	}
	return append(dirs, originalDir)
}

// mountOverlay (re)mounts an OverlayFS merged view at mountPoint, with the
// runtime pseudo-filesystems mounted beneath it. lowerDirs are ordered
// highest-priority first. Any previous mounts at mountPoint are torn down.
func mountOverlay(lowerDirs []string, upperDir, workDir, mountPoint string) error {
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		return err
	}
	unmountRuntimeFS(mountPoint)
	_ = unix.Unmount(mountPoint, unix.MNT_DETACH) // best-effort: may not be mounted

	options := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
		strings.Join(lowerDirs, ":"), upperDir, workDir)
	if err := unix.Mount("overlay", mountPoint, "overlay", 0, options); err != nil {
		return fmt.Errorf("mount overlay at %s failed: %w", mountPoint, err)
	}

	if err := mountRuntimeFS(mountPoint); err != nil {
		_ = unix.Unmount(mountPoint, unix.MNT_DETACH)
		return fmt.Errorf("mount runtime filesystems failed: %w", err)
	}
	return nil
}

// runtimeMounts are the pseudo-filesystems every merged view needs so that
// processes inside it (and CRIU) can resolve /proc and /sys.
var runtimeMounts = []struct {
	rel    string
	source string
	fstype string
}{
	{rel: "proc", source: "proc", fstype: "proc"},
	{rel: "sys", source: "sys", fstype: "sysfs"},
}

func mountRuntimeFS(mountPoint string) error {
	for _, rm := range runtimeMounts {
		target := filepath.Join(mountPoint, rm.rel)
		if err := os.MkdirAll(target, 0o755); err != nil {
			return fmt.Errorf("mkdir runtime mount target %s failed: %w", target, err)
		}
		if err := unix.Mount(rm.source, target, rm.fstype, 0, ""); err != nil {
			if err == syscall.EBUSY {
				continue // already mounted
			}
			return fmt.Errorf("mount %s on %s failed: %w", rm.fstype, target, err)
		}
	}
	return nil
}

func unmountRuntimeFS(mountPoint string) {
	if strings.TrimSpace(mountPoint) == "" {
		return
	}
	for _, rm := range runtimeMounts {
		target := filepath.Join(mountPoint, rm.rel)
		if err := unix.Unmount(target, 0); err != nil {
			if err == syscall.EINVAL || err == syscall.ENOENT {
				continue // not mounted
			}
			_ = unix.Unmount(target, unix.MNT_DETACH)
		}
	}
}
