package waypoint

// Where a checkpoint's CRIU images physically live.
//
// checkpoints/<ckpt>/criu is the one path every consumer uses (dump -D,
// restore --images-dir, stats parsing, size accounting). With TmpfsImages
// enabled it is a symlink: first to a tmpfs dir the dump writes into (so the
// frozen window pays memcpy speed, not NVMe writeback), later — after a
// detached flusher has copied and fsynced the images to
// checkpoints/<ckpt>/criu.disk — atomically repointed there, and the tmpfs
// copy is deleted to give the RAM back. The repoint+delete is guarded by a
// per-checkpoint flock that concurrent restores hold shared, so images can
// never disappear under a running `criu restore`.
//
// Crash behavior: if the flusher dies the symlink keeps pointing at tmpfs
// and everything still works (until reboot); `cleanup` removes the session's
// tmpfs dir either way. The window where a host reboot loses a checkpoint is
// dump-end -> flush-end.

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// diskImagesDirName is the durable images dir a flush produces, next to the
// `criu` symlink inside the checkpoint dir.
const diskImagesDirName = "criu.disk"

func (m *Manager) sessionTmpfsDir() string {
	return filepath.Join(TmpfsImagesDir, m.sessionID)
}

func (m *Manager) checkpointTmpfsDir(checkpointID string) string {
	return filepath.Join(m.sessionTmpfsDir(), checkpointID)
}

// prepareCheckpointImagesDir creates the images dir a dump will write into.
// Plain mode: the real checkpoints/<ckpt>/criu directory. Tmpfs mode: a
// tmpfs dir with checkpoints/<ckpt>/criu as a symlink to it. Either way the
// returned path is the canonical criu dir.
func (m *Manager) prepareCheckpointImagesDir(checkpointID string) (string, error) {
	criuDir := m.checkpointCriuDir(checkpointID)
	if !TmpfsImages {
		return criuDir, os.MkdirAll(criuDir, 0o755)
	}
	tmpfsDir := m.checkpointTmpfsDir(checkpointID)
	if err := os.MkdirAll(tmpfsDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir tmpfs images dir %s failed: %w", tmpfsDir, err)
	}
	if err := os.MkdirAll(filepath.Dir(criuDir), 0o755); err != nil {
		return "", err
	}
	if err := os.Symlink(tmpfsDir, criuDir); err != nil {
		return "", fmt.Errorf("symlink %s -> %s failed: %w", criuDir, tmpfsDir, err)
	}
	return criuDir, nil
}

// imagesLockPath is the per-checkpoint flock guarding the images location:
// restores hold it shared for the duration of `criu restore`, the flusher
// holds it exclusive only while repointing the symlink and deleting tmpfs.
func imagesLockPath(ckptDir string) string {
	return filepath.Join(ckptDir, "images.lock")
}

// lockImages takes the images flock; the returned func releases it. Callers
// treat errors as best-effort (a missing checkpoint dir means there is
// nothing to guard against).
func lockImages(ckptDir string, exclusive bool) (func(), error) {
	f, err := os.OpenFile(imagesLockPath(ckptDir), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	how := unix.LOCK_SH
	if exclusive {
		how = unix.LOCK_EX
	}
	if err := unix.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}

// spawnImageFlusher starts the detached flush helper for a checkpoint. Each
// CLI command is a short-lived process, so "async" means a re-exec that
// outlives us (same pattern as the restore helper). Best-effort: on failure
// the images simply stay on tmpfs.
func (m *Manager) spawnImageFlusher(checkpointID string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(m.checkpointDir(checkpointID), "flush.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(exe, "__waypoint_flush_images", m.sessionID, checkpointID)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// RunImageFlushFromArgs is the entry point for the hidden
// __waypoint_flush_images CLI subcommand.
func RunImageFlushFromArgs(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: __waypoint_flush_images <session> <checkpoint-id>")
	}
	m, err := LoadManager(args[0])
	if err != nil {
		return err
	}
	return m.FlushCheckpointImages(args[1])
}

// FlushCheckpointImages copies a checkpoint's tmpfs images to the durable
// criu.disk dir (fsynced), atomically repoints the criu symlink there, and
// deletes the tmpfs copy. Idempotent: a checkpoint already on disk is a
// no-op.
func (m *Manager) FlushCheckpointImages(checkpointID string) error {
	if err := validateCheckpointID(checkpointID); err != nil {
		return err
	}
	ckptDir := m.checkpointDir(checkpointID)
	criuLink := m.checkpointCriuDir(checkpointID)

	target, err := os.Readlink(criuLink)
	if err != nil {
		// A real directory (tmpfs mode off) or a missing checkpoint:
		// nothing to flush.
		return nil
	}
	tmpfsDir := m.checkpointTmpfsDir(checkpointID)
	if target != tmpfsDir {
		return nil // already repointed to the disk copy
	}

	diskDir := filepath.Join(ckptDir, diskImagesDirName)
	if err := os.RemoveAll(diskDir); err != nil {
		return err
	}
	if err := os.MkdirAll(diskDir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(tmpfsDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		if err := copyFileSync(filepath.Join(tmpfsDir, e.Name()), filepath.Join(diskDir, e.Name())); err != nil {
			return fmt.Errorf("flush %s: %w", e.Name(), err)
		}
	}
	if err := syncDir(diskDir); err != nil {
		return err
	}

	// Repoint + delete under the exclusive lock so no restore is mid-read.
	unlock, err := lockImages(ckptDir, true)
	if err != nil {
		return fmt.Errorf("images lock: %w", err)
	}
	defer unlock()

	// Relative target keeps the session tree relocatable.
	newLink := criuLink + ".new"
	_ = os.Remove(newLink)
	if err := os.Symlink(diskImagesDirName, newLink); err != nil {
		return err
	}
	if err := os.Rename(newLink, criuLink); err != nil {
		return err
	}
	if err := syncDir(ckptDir); err != nil {
		return err
	}
	if err := os.RemoveAll(tmpfsDir); err != nil {
		return err
	}
	_ = os.Remove(m.sessionTmpfsDir()) // rmdir if this was the last checkpoint
	return nil
}

func copyFileSync(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
