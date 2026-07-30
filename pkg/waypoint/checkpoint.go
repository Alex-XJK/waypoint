package waypoint

// Immutable checkpoints and the Materializer that turns checkpoints into
// live forks (CRIU restore) and live forks into checkpoints (CRIU dump +
// sealing the fork's overlay upper into a new layer).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// Metadata represents the metadata stored for each immutable checkpoint.
// ParentID is the logical DAG edge. LayerIDs is the resolved filesystem layer
// chain from oldest to newest and includes this checkpoint's ID.
type Metadata struct {
	ID                string           `json:"id"`
	ParentID          string           `json:"parent_id,omitempty"`
	LayerIDs          []string         `json:"layer_ids"`
	PID               int              `json:"pid"`
	OriginalDir       string           `json:"original_dir"`
	SessionID         string           `json:"session_id"`
	CreatedFromForkID string           `json:"created_from_fork_id,omitempty"`
	CreatedAt         int64            `json:"created_at"`
	Status            CheckpointStatus `json:"status"`
	// Snapshot is the phase breakdown of the snapshot that created this
	// checkpoint (instrumentation, not part of the logical state).
	Snapshot *SnapshotBreakdown `json:"snapshot_breakdown,omitempty"`
}

type CheckpointStatus string

const (
	CheckpointStatusCreating CheckpointStatus = "creating"
	CheckpointStatusReady    CheckpointStatus = "ready"
	CheckpointStatusFailed   CheckpointStatus = "failed"
)

// Checkpoint is a loaded checkpoint: metadata plus resolved paths.
type Checkpoint struct {
	ID       string
	Dir      string
	CriuPath string
	Metadata *Metadata
}

// ForkSpec describes a fork to be materialized.
type ForkSpec struct {
	ID string
}

// Materializer converts checkpoints to live forks and back. CRIU is the only
// implementation today; DeltaBox-style frozen templates would be another.
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

func (m *Manager) saveMetadata(checkpointID string, metadata Metadata) error {
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(m.metadataDir, checkpointID+".json"), data, 0o644)
}

func (m *Manager) loadMetadata(checkpointID string) (*Metadata, error) {
	data, err := os.ReadFile(filepath.Join(m.metadataDir, checkpointID+".json"))
	if err != nil {
		return nil, err
	}

	var metadata Metadata
	err = json.Unmarshal(data, &metadata)
	return &metadata, err
}

// LoadCheckpointMetadata returns persisted metadata for a session checkpoint
// without needing a live Manager.
func LoadCheckpointMetadata(sessionID, checkpointID string) (*Metadata, error) {
	sessionInfo, err := loadSessionInfo(sessionID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(sessionInfo.BaseDir, "metadata", checkpointID+".json"))
	if err != nil {
		return nil, err
	}

	var metadata Metadata
	err = json.Unmarshal(data, &metadata)
	return &metadata, err
}

func (m *Manager) LoadCheckpoint(checkpointID string) (*Checkpoint, error) {
	metadata, err := m.loadMetadata(checkpointID)
	if err != nil {
		return nil, fmt.Errorf("failed to load checkpoint metadata: %w", err)
	}
	return &Checkpoint{
		ID:       checkpointID,
		Dir:      m.checkpointDir(checkpointID),
		CriuPath: m.checkpointCriuDir(checkpointID),
		Metadata: metadata,
	}, nil
}

// ForkCheckpoint materializes a new live fork from a checkpoint.
func (m *Manager) ForkCheckpoint(checkpointID string, spec ForkSpec) (*Fork, error) {
	ckpt, err := m.LoadCheckpoint(checkpointID)
	if err != nil {
		return nil, err
	}
	return NewCRIUMaterializer(m).Materialize(ckpt, spec)
}

// SnapshotFork turns a live fork into a new checkpoint, then resumes the fork
// on top of it.
func (m *Manager) SnapshotFork(forkID, checkpointID string) (*Checkpoint, error) {
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

// ParkFork turns a live fork into a new checkpoint and releases the fork
// instead of resuming it: the dump already stopped the tree, its upper is
// sealed as the checkpoint's delta layer, and the fork's runtime dirs are
// removed. The node survives only as the checkpoint and can be revived later
// with ForkCheckpoint (any fork ID, including the parked one, is free again).
// This is the cheapest persist operation — no re-restore leg — intended for
// leaving a search-tree node that won't continue running.
func (m *Manager) ParkFork(forkID, checkpointID string) (*Checkpoint, error) {
	if forkID == MainForkID {
		return nil, fmt.Errorf("cannot park %s: the session needs a live main fork", MainForkID)
	}
	var ckpt *Checkpoint
	err := m.withForkLock(forkID, func() error {
		f, err := m.loadFork(forkID)
		if err != nil {
			return err
		}
		ckpt, err = m.snapshotFork(f, checkpointID, true)
		return err
	})
	return ckpt, err
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

	// Allocate the fork record under the session lock; restore under the
	// fork lock so long CRIU work never blocks the whole session.
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
		bd, err := m.restoreForkAndWait(f)
		if PhaseStats {
			f.RestoreBreakdown = bd // keep partial phases on failure for debugging
		}
		if err != nil {
			f.Status = ForkStatusFailed
			_ = m.saveFork(f)
			return err
		}
		f.RestoreDuration = time.Since(start).String()
		bd.TotalMs = durMs(time.Since(start))
		f.Status = ForkStatusRunning
		return m.saveFork(f)
	}); err != nil {
		return nil, err
	}
	return f, nil
}

// restoreForkAndWait runs the restore helper for f, waits for the fork's
// shell socket, and assembles the phase breakdown from our own wall clocks,
// the helper child's timing file, and criu's stats-restore image. The caller
// must hold the fork lock. On error the returned breakdown carries whatever
// phases completed.
func (m *Manager) restoreForkAndWait(f *Fork) (*RestoreBreakdown, error) {
	bd := &RestoreBreakdown{}
	helperStart := time.Now()
	if err := runRestoreHelper(f); err != nil {
		return bd, err
	}
	bd.HelperMs = durMs(time.Since(helperStart))
	if PhaseStats {
		if t, err := readChildRestoreTiming(f); err == nil {
			bd.MountMs, bd.CriuWallMs = t.MountMs, t.CriuWallMs
		}
		if rs, err := readCriuRestoreStats(filepath.Join(f.RootDir, "stats-restore")); err == nil {
			bd.CriuForkMs = rs.ForkingMs
			bd.CriuRestoreMs = rs.RestoreMs
			bd.PagesRestored = rs.PagesRestored
		}
	}

	if pid, err := readPIDFile(f.PidFile); err == nil {
		f.PID = pid
		f.SocketPath = socketPathThroughProcRoot(pid, f.CanonicalSocket)
	}
	sockStart := time.Now()
	if err := waitForForkSocket(f.SocketPath, 5*time.Second); err != nil {
		return bd, err
	}
	bd.SockWaitMs = durMs(time.Since(sockStart))
	return bd, nil
}

func (c *CRIUMaterializer) Snapshot(f *Fork, id string) (*Checkpoint, error) {
	return c.manager.snapshotFork(f, id, false)
}

// snapshotFork dumps the fork's process tree and seals its upper dir into the
// new checkpoint's layer. With park=false it then rebases the fork onto that
// checkpoint and restores it; with park=true the fork is released instead
// (the dump already stopped the tree) and only the checkpoint remains. The
// caller must hold the fork lock.
func (m *Manager) snapshotFork(f *Fork, checkpointID string, park bool) (*Checkpoint, error) {
	if checkpointID == "" || checkpointID == "current" {
		return nil, fmt.Errorf("invalid checkpoint ID: %s", checkpointID)
	}
	if f.Status != ForkStatusRunning {
		return nil, fmt.Errorf("fork %s is not running (status=%s)", f.ID, f.Status)
	}
	if f.PID <= 0 {
		return nil, fmt.Errorf("fork %s has no live PID", f.ID)
	}
	if park && f.ID == MainForkID {
		return nil, fmt.Errorf("cannot park %s: the session needs a live main fork", MainForkID)
	}

	layerIDs := append(append([]string(nil), f.LayerIDs...), checkpointID)
	metadata := Metadata{
		ID:                checkpointID,
		ParentID:          f.BaseCheckpointID,
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
		if _, err := m.prepareCheckpointImagesDir(checkpointID); err != nil {
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
	snapStart := time.Now()
	dumpStart := time.Now()
	if err := m.createMemoryCheckpoint(f.PID, ckptCriu); err != nil {
		return fail(fmt.Errorf("memory checkpoint failed: %w", err))
	}
	dumpMs := durMs(time.Since(dumpStart))
	if TmpfsImages {
		// Persist the tmpfs images in the background; forks meanwhile read
		// them straight from tmpfs through the criu symlink.
		if err := m.spawnImageFlusher(checkpointID); err != nil {
			fmt.Fprintf(os.Stderr, "warning: image flush not started for %s (images stay on tmpfs): %v\n", checkpointID, err)
		}
	}

	sealStart := time.Now()
	if f.ID == MainForkID {
		unmountRuntimeFS(f.MountPoint)
		_ = unix.Unmount(f.MountPoint, unix.MNT_DETACH)
	}

	if err := os.Rename(f.UpperDir, ckptUpper); err != nil {
		return fail(fmt.Errorf("seal fork upper failed: %w", err))
	}

	if park {
		// The dump already stopped the tree and the checkpoint now owns the
		// memory image and the sealed delta layer; discard the fork's runtime
		// remains instead of re-restoring it. A cleanup failure doesn't fail
		// the park — the checkpoint is already durable.
		_ = os.Remove(f.SocketPath)
		if err := os.RemoveAll(f.RootDir); err != nil {
			fmt.Fprintf(os.Stderr, "warning: parked fork %s left runtime dir behind: %v\n", f.ID, err)
		}
		if PhaseStats {
			breakdown := &SnapshotBreakdown{
				DumpMs:  dumpMs,
				SealMs:  durMs(time.Since(sealStart)),
				TotalMs: durMs(time.Since(snapStart)),
			}
			if ds, err := readCriuDumpStats(filepath.Join(ckptCriu, "stats-dump")); err == nil {
				breakdown.Dump = ds
			}
			metadata.Snapshot = breakdown
		}
		metadata.Status = CheckpointStatusReady
		if err := m.withSessionLock(func() error {
			return m.saveMetadata(checkpointID, metadata)
		}); err != nil {
			return nil, err
		}
		return &Checkpoint{
			ID:       checkpointID,
			Dir:      ckptDir,
			CriuPath: ckptCriu,
			Metadata: &metadata,
		}, nil
	}

	if err := os.RemoveAll(f.WorkDir); err != nil {
		return fail(fmt.Errorf("remove old fork workdir failed: %w", err))
	}
	for _, dir := range []string{f.UpperDir, f.WorkDir, f.TempDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fail(fmt.Errorf("mkdir %s failed: %w", dir, err))
		}
	}
	sealMs := durMs(time.Since(sealStart))

	f.BaseCheckpointID = checkpointID
	f.LayerIDs = layerIDs
	f.CriuPath = ckptCriu
	f.PidFile = filepath.Join(f.RootDir, "restore.pid")
	_ = os.Remove(f.PidFile)
	if err := m.saveFork(f); err != nil {
		return fail(err)
	}
	restoreStart := time.Now()
	bd, err := m.restoreForkAndWait(f)
	if PhaseStats {
		f.RestoreBreakdown = bd // keep partial phases on failure for debugging
	}
	if err != nil {
		return fail(err)
	}
	f.Status = ForkStatusRunning
	f.RestoreDuration = ""
	if err := m.saveFork(f); err != nil {
		return fail(err)
	}

	if PhaseStats {
		breakdown := &SnapshotBreakdown{
			DumpMs:    dumpMs,
			SealMs:    sealMs,
			RestoreMs: durMs(time.Since(restoreStart)),
			Restore:   bd,
			TotalMs:   durMs(time.Since(snapStart)),
		}
		if ds, err := readCriuDumpStats(filepath.Join(ckptCriu, "stats-dump")); err == nil {
			breakdown.Dump = ds
		}
		metadata.Snapshot = breakdown
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
