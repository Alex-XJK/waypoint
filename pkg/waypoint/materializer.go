package waypoint

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type Checkpoint struct {
	ID       string
	Dir      string
	CriuPath string
	Metadata *Metadata
}

type ForkSpec struct {
	ID        string
	LazyPages bool
}

type Materializer interface {
	Materialize(ckpt *Checkpoint, spec ForkSpec) (*Fork, error)
	Snapshot(f *Fork, id string) (*Checkpoint, error)
}

type CRIUMaterializer struct {
	manager *Manager
}

func NewCRIUMaterializer(m *Manager) *CRIUMaterializer {
	return &CRIUMaterializer{manager: m}
}

func (m *Manager) LoadCheckpoint(checkpointID string) (*Checkpoint, error) {
	metadata, err := m.loadMetadata(checkpointID)
	if err != nil {
		return nil, fmt.Errorf("failed to load checkpoint metadata: %w", err)
	}
	dir := m.checkpointDir(checkpointID)
	return &Checkpoint{
		ID:       checkpointID,
		Dir:      dir,
		CriuPath: m.checkpointCriuDir(checkpointID),
		Metadata: metadata,
	}, nil
}

func (c *CRIUMaterializer) Materialize(ckpt *Checkpoint, spec ForkSpec) (*Fork, error) {
	m := c.manager
	if ckpt == nil || ckpt.Metadata == nil {
		return nil, fmt.Errorf("checkpoint is nil")
	}
	if ckpt.Metadata.PID == SkipMemoryCheckpoint {
		return nil, fmt.Errorf("checkpoint %s has no memory image to fork", ckpt.ID)
	}
	if _, err := os.Stat(ckpt.CriuPath); err != nil {
		return nil, fmt.Errorf("checkpoint %s CRIU images not found: %w", ckpt.ID, err)
	}

	var f *Fork
	if err := m.withSessionLock(func() error {
		var err error
		f, err = newForkRecord(m, ckpt.ID, ckpt.Metadata, spec)
		if err != nil {
			return err
		}
		if _, err := os.Stat(f.RootDir); err == nil {
			return fmt.Errorf("fork %s already exists", f.ID)
		} else if !os.IsNotExist(err) {
			return err
		}
		for _, dir := range []string{f.UpperDir, f.WorkDir, f.TempDir} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("mkdir %s failed: %w", dir, err)
			}
		}
		return m.saveFork(f)
	}); err != nil {
		return nil, err
	}

	start := time.Now()
	if err := m.withForkLock(f.ID, func() error {
		if err := c.runRestoreHelper(f); err != nil {
			f.Status = ForkStatusDestroyed
			_ = m.saveFork(f)
			return err
		}
		if pid, err := readPIDFile(f.PidFile); err == nil {
			f.PID = pid
			f.SocketPath = socketPathThroughProcRoot(pid, f.CanonicalSocket)
		}
		if err := waitForForkSocket(f.SocketPath, 5*time.Second); err != nil {
			f.Status = ForkStatusDestroyed
			_ = m.saveFork(f)
			return err
		}
		f.RestoreDuration = time.Since(start).String()
		f.Status = ForkStatusRunning
		return m.saveFork(f)
	}); err != nil {
		return nil, err
	}
	return f, nil
}

func (c *CRIUMaterializer) Snapshot(f *Fork, id string) (*Checkpoint, error) {
	return c.manager.snapshotFork(f, id)
}

func (c *CRIUMaterializer) runRestoreHelper(f *Fork) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	statePath := filepath.Join(f.RootDir, ForkStateFile)
	cmd := exec.Command(exe, "__waypoint_restore_fork_child", statePath)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: uintptr(unix.CLONE_NEWNS | unix.CLONE_NEWNET | unix.CLONE_NEWIPC),
		Setsid:     true,
	}
	var stderr bytes.Buffer
	cmd.Stdout = &stderr
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("fork restore helper failed: %w\n%s", err, stderr.String())
	}
	return nil
}

func (m *Manager) ForkCheckpoint(checkpointID string, spec ForkSpec) (*Fork, error) {
	if err := EnsureCriuCompatible(); err != nil {
		return nil, err
	}
	ckpt, err := m.LoadCheckpoint(checkpointID)
	if err != nil {
		return nil, err
	}
	return NewCRIUMaterializer(m).Materialize(ckpt, spec)
}

func (m *Manager) SnapshotFork(forkID, checkpointID string) (*Checkpoint, error) {
	if err := EnsureCriuCompatible(); err != nil {
		return nil, err
	}
	var ckpt *Checkpoint
	err := m.withForkLock(forkID, func() error {
		f, err := m.loadFork(forkID)
		if err != nil {
			return err
		}
		ckpt, err = NewCRIUMaterializer(m).Snapshot(f, checkpointID)
		return err
	})
	return ckpt, err
}

func (m *Manager) snapshotFork(f *Fork, checkpointID string) (*Checkpoint, error) {
	if checkpointID == "" || checkpointID == "current" {
		return nil, fmt.Errorf("invalid checkpoint ID: %s", checkpointID)
	}
	if f.Status != ForkStatusRunning {
		return nil, fmt.Errorf("fork %s is not running (status=%s)", f.ID, f.Status)
	}
	if f.PID <= 0 {
		return nil, fmt.Errorf("fork %s has no live PID", f.ID)
	}

	parentID := f.BaseCheckpointID
	layerIDs := append(append([]string(nil), f.LayerIDs...), checkpointID)
	metadata := Metadata{
		ID:                checkpointID,
		ParentID:          parentID,
		LayerIDs:          layerIDs,
		PID:               f.PID,
		OriginalDir:       f.OriginalDir,
		SessionID:         f.SessionID,
		CreatedFromForkID: f.ID,
		CreatedAt:         time.Now().Unix(),
		Status:            CheckpointStatusCreating,
	}
	ckptDir := m.checkpointDir(checkpointID)
	ckptUpper := m.checkpointUpperDir(checkpointID)
	ckptCriu := m.checkpointCriuDir(checkpointID)

	if err := m.withSessionLock(func() error {
		if _, err := os.Stat(ckptDir); err == nil {
			return fmt.Errorf("checkpoint %s already exists", checkpointID)
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.MkdirAll(ckptCriu, 0o755); err != nil {
			return err
		}
		return m.saveMetadata(checkpointID, metadata)
	}); err != nil {
		return nil, err
	}

	fail := func(err error) (*Checkpoint, error) {
		metadata.Status = CheckpointStatusFailed
		_ = m.saveMetadata(checkpointID, metadata)
		f.Status = ForkStatusFailed
		_ = m.saveFork(f)
		return nil, err
	}

	f.Status = ForkStatusSnapshot
	if err := m.saveFork(f); err != nil {
		return fail(err)
	}
	if err := m.createMemoryCheckpoint(f.PID, ckptCriu); err != nil {
		return fail(fmt.Errorf("memory checkpoint failed: %w", err))
	}

	if f.ID == MainForkID {
		m.unmountRuntimeFS(f.MountPoint)
		_ = unix.Unmount(f.MountPoint, unix.MNT_DETACH)
	}

	if err := os.Rename(f.UpperDir, ckptUpper); err != nil {
		return fail(fmt.Errorf("seal fork upper failed: %w", err))
	}
	if err := os.RemoveAll(f.WorkDir); err != nil {
		return fail(fmt.Errorf("remove old fork workdir failed: %w", err))
	}
	for _, dir := range []string{f.UpperDir, f.WorkDir, f.TempDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fail(fmt.Errorf("mkdir %s failed: %w", dir, err))
		}
	}

	f.BaseCheckpointID = checkpointID
	f.LayerIDs = layerIDs
	f.CriuPath = ckptCriu
	f.PidFile = filepath.Join(f.RootDir, "restore.pid")
	_ = os.Remove(f.PidFile)
	if err := m.saveFork(f); err != nil {
		return fail(err)
	}
	if err := NewCRIUMaterializer(m).runRestoreHelper(f); err != nil {
		return fail(err)
	}
	if pid, err := readPIDFile(f.PidFile); err == nil {
		f.PID = pid
		f.SocketPath = socketPathThroughProcRoot(pid, f.CanonicalSocket)
	}
	if err := waitForForkSocket(f.SocketPath, 5*time.Second); err != nil {
		return fail(err)
	}
	f.Status = ForkStatusRunning
	f.RestoreDuration = ""
	if err := m.saveFork(f); err != nil {
		return fail(err)
	}

	metadata.PID = f.PID
	metadata.Status = CheckpointStatusReady
	if err := m.withSessionLock(func() error {
		return m.saveMetadata(checkpointID, metadata)
	}); err != nil {
		return nil, err
	}
	if f.ID == MainForkID {
		m.shellPid = f.PID
		m.shellSocket = f.SocketPath
		_ = saveSessionInfo(m.sessionID, m)
	}

	return &Checkpoint{
		ID:       checkpointID,
		Dir:      ckptDir,
		CriuPath: ckptCriu,
		Metadata: &metadata,
	}, nil
}

func (m *Manager) ExecuteForkCommand(forkID, command string, args ...string) (string, error) {
	var output string
	err := m.withForkLock(forkID, func() error {
		f, err := m.loadFork(forkID)
		if err != nil {
			return err
		}
		if f.Status != ForkStatusRunning {
			return fmt.Errorf("fork %s is not running (status=%s)", forkID, f.Status)
		}
		commandString := command
		if len(args) > 0 {
			commandString += " " + strings.Join(args, " ")
		}
		commandString += "\n"
		var execErr error
		output, execErr = execCommand(f.SocketPath, commandString)
		return execErr
	})
	return output, err
}

func (m *Manager) DestroyFork(forkID string) error {
	return m.withForkLock(forkID, func() error {
		f, err := m.loadFork(forkID)
		if err != nil {
			return err
		}
		f.Status = ForkStatusDestroyed
		if err := m.saveFork(f); err != nil {
			return err
		}
		if f.PID > 0 {
			if err := m.killProcess(f.PID); err != nil {
				return err
			}
		}
		_ = os.Remove(f.SocketPath)
		return os.RemoveAll(f.RootDir)
	})
}

func RunForkRestoreChildFromArgs(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: __waypoint_restore_fork_child <fork-state-json>")
	}
	f, err := loadForkFile(args[0])
	if err != nil {
		return err
	}
	return restoreForkChild(f)
}

func restoreForkChild(f *Fork) error {
	if err := prepareForkMountNamespace(f); err != nil {
		return err
	}
	if err := bringLoopbackUp(); err != nil {
		return err
	}

	args := []string{
		"restore",
		"--images-dir", f.CriuPath,
		"--tcp-established",
		"--restore-detached",
		"--pidfile", f.PidFile,
		// Absolute path: criu resolves relative -o against the images dir,
		// which is shared by all forks of a checkpoint and would make
		// concurrent restores clobber one another's logs.
		"-vv", "-o", f.LogPath,
	}
	if f.LazyPages {
		args = append(args, "--lazy-pages")
	}
	if f.MountPoint != "" {
		args = append(args, "-r", f.MountPoint)
		args = append(args, "--external", fmt.Sprintf("mnt[waypoint-work]:%s", f.MountPoint))
	}
	cmd := exec.Command("criu", args...)
	cmd.Dir = f.RootDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("criu restore failed: %w\n%s", err, stderr.String())
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pid, err := readPIDFile(f.PidFile); err == nil && pid > 0 {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("criu restore did not write pidfile %s", f.PidFile)
}

func prepareForkMountNamespace(f *Fork) error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mount namespace private failed: %w", err)
	}

	if err := mountOverlayAt(f.LayerIDs, f.UpperDir, f.WorkDir, f.MountPoint, f.SessionID, f.RootDir, f.OriginalDir); err != nil {
		return err
	}
	return nil
}

func mountOverlayAt(layerIDs []string, upperDir, workDir, mountPoint, sessionID, rootDir, originalDir string) error {
	sessionDir := filepath.Dir(filepath.Dir(rootDir))
	lowerDirs := make([]string, 0, len(layerIDs)+1)
	for i := len(layerIDs) - 1; i >= 0; i-- {
		lowerDirs = append(lowerDirs, filepath.Join(sessionDir, "checkpoints", layerIDs[i], "upper"))
	}
	lowerDirs = append(lowerDirs, originalDir)

	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		return err
	}
	_ = unix.Unmount(filepath.Join(mountPoint, "proc"), unix.MNT_DETACH)
	_ = unix.Unmount(filepath.Join(mountPoint, "sys"), unix.MNT_DETACH)
	_ = unix.Unmount(mountPoint, unix.MNT_DETACH)

	options := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", joinStrings(lowerDirs, ":"), upperDir, workDir)
	if err := unix.Mount("overlay", mountPoint, "overlay", 0, options); err != nil {
		return fmt.Errorf("mount overlay for fork %s failed: %w", sessionID, err)
	}
	if err := mountRuntimeFSAt(mountPoint); err != nil {
		_ = unix.Unmount(mountPoint, unix.MNT_DETACH)
		return err
	}
	return nil
}

func mountRuntimeFSAt(mountPoint string) error {
	for _, mount := range []struct {
		rel    string
		source string
		fstype string
	}{
		{rel: "proc", source: "proc", fstype: "proc"},
		{rel: "sys", source: "sys", fstype: "sysfs"},
	} {
		target := filepath.Join(mountPoint, mount.rel)
		if err := os.MkdirAll(target, 0o755); err != nil {
			return err
		}
		if err := unix.Mount(mount.source, target, mount.fstype, 0, ""); err != nil && err != syscall.EBUSY {
			return fmt.Errorf("mount %s on %s failed: %w", mount.fstype, target, err)
		}
	}
	return nil
}

func bringLoopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP | unix.IFF_RUNNING)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}

func joinStrings(values []string, sep string) string {
	if len(values) == 0 {
		return ""
	}
	out := values[0]
	for _, value := range values[1:] {
		out += sep + value
	}
	return out
}

func waitForForkSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("socket %s did not appear", path)
}

func socketPathThroughProcRoot(pid int, canonicalSocket string) string {
	return filepath.Join("/proc", strconv.Itoa(pid), "root", strings.TrimPrefix(canonicalSocket, string(filepath.Separator)))
}
