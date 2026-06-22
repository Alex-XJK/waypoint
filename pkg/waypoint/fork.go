package waypoint

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const ForkStateFile = "fork.json"

type ForkStatus string

const (
	ForkStatusStarting  ForkStatus = "starting"
	ForkStatusRunning   ForkStatus = "running"
	ForkStatusDestroyed ForkStatus = "destroyed"
)

type Fork struct {
	ID               string     `json:"id"`
	SessionID        string     `json:"session_id"`
	CheckpointID     string     `json:"checkpoint_id"`
	OriginalDir      string     `json:"original_dir"`
	RootDir          string     `json:"root_dir"`
	UpperDir         string     `json:"upper_dir"`
	WorkDir          string     `json:"work_dir"`
	TempDir          string     `json:"temp_dir"`
	CanonicalTempDir string     `json:"canonical_temp_dir"`
	MountPoint       string     `json:"mount_point"`
	SocketPath       string     `json:"socket_path"`
	CanonicalSocket  string     `json:"canonical_socket"`
	LogPath          string     `json:"log_path"`
	CriuPath         string     `json:"criu_path"`
	PidFile          string     `json:"pid_file"`
	PID              int        `json:"pid"`
	ParentList       []string   `json:"parent_list"`
	CreatedAt        int64      `json:"created_at"`
	RestoreDuration  string     `json:"restore_duration,omitempty"`
	Status           ForkStatus `json:"status"`
	LazyPages        bool       `json:"lazy_pages,omitempty"`
}

func (m *Manager) forksDir() string {
	return filepath.Join(m.baseDir, "forks")
}

func (m *Manager) forkDir(forkID string) string {
	return filepath.Join(m.forksDir(), forkID)
}

func (m *Manager) newForkID() (string, error) {
	id, err := generateSessionID()
	if err != nil {
		return "", err
	}
	return "fork-" + id, nil
}

func (m *Manager) saveFork(f *Fork) error {
	if err := os.MkdirAll(f.RootDir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(f.RootDir, ForkStateFile), data, 0o644)
}

func (m *Manager) loadFork(forkID string) (*Fork, error) {
	path := filepath.Join(m.forkDir(forkID), ForkStateFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f Fork
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

func loadForkFile(path string) (*Fork, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f Fork
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

func readPIDFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("invalid pidfile %s: %w", path, err)
	}
	return pid, nil
}

func (m *Manager) ListForks() ([]*Fork, error) {
	entries, err := os.ReadDir(m.forksDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	forks := make([]*Fork, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		f, err := m.loadFork(entry.Name())
		if err != nil {
			continue
		}
		forks = append(forks, f)
	}
	return forks, nil
}

func newForkRecord(m *Manager, checkpointID string, metadata *Metadata, spec ForkSpec) (*Fork, error) {
	forkID := spec.ID
	if forkID == "" {
		var err error
		forkID, err = m.newForkID()
		if err != nil {
			return nil, err
		}
	}

	rootDir := m.forkDir(forkID)
	tempDir := filepath.Join(rootDir, "temp")
	originalDir := metadata.OriginalDir
	if originalDir == "" {
		originalDir = m.originalDir
	}
	canonicalSocket := filepath.Join(m.baseDir, "temp", fmt.Sprintf("shell_%s.sock", m.sessionID))
	socketPath := filepath.Join(tempDir, filepath.Base(canonicalSocket))

	return &Fork{
		ID:               forkID,
		SessionID:        m.sessionID,
		CheckpointID:     checkpointID,
		OriginalDir:      originalDir,
		RootDir:          rootDir,
		UpperDir:         filepath.Join(rootDir, "upper"),
		WorkDir:          filepath.Join(rootDir, "work"),
		TempDir:          tempDir,
		CanonicalTempDir: filepath.Dir(canonicalSocket),
		MountPoint:       m.workOverlay,
		SocketPath:       socketPath,
		CanonicalSocket:  canonicalSocket,
		LogPath:          filepath.Join(rootDir, "restore.log"),
		CriuPath:         filepath.Join(m.baseDir, checkpointID, "criu"),
		PidFile:          filepath.Join(rootDir, "restore.pid"),
		ParentList:       append([]string(nil), metadata.ParentList...),
		CreatedAt:        time.Now().Unix(),
		Status:           ForkStatusStarting,
		LazyPages:        spec.LazyPages,
	}, nil
}
