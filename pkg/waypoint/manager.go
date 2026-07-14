package waypoint

// The Manager handle: configuration, session lifecycle, the global session
// registry, and the session's on-disk layout (paths, locks, atomic writes).

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Manager manages runtime checkpoint sessions, the main struct.
type Manager struct {
	baseDir     string // Base directory for this session, e.g., /tmp/waypoint-sessions/a1b2c3d4e5f6g7h8
	metadataDir string // Directory for metadata files, e.g., <baseDir>/metadata
	workOverlay string // Current working overlay mount point, e.g., <baseDir>/work
	originalDir string // Original directory being managed, e.g., /home/user/app-data
	sessionID   string // Unique session identifier, e.g., a1b2c3d4e5f6g7h8
	shellPid    int    // PID of the shell process if a shell is enabled, ShellNotEnabled(=0) otherwise
	shellSocket string // Path to the shell socket if enabled, empty otherwise
}

// SessionInfo holds information about a checkpoint session.
// It is serialized to JSON and stored in a globally known location for session tracking.
type SessionInfo struct {
	SessionID   string `json:"session_id"`
	BaseDir     string `json:"base_dir"`
	OriginalDir string `json:"original_dir"`
	WorkOverlay string `json:"work_overlay"`
	CreatedAt   int64  `json:"created_at"`
	ShellPid    int    `json:"shell_pid"`
	ShellSocket string `json:"shell_socket,omitempty"`
}

// PID values for special cases
const SkipMemoryCheckpoint = -1 // Checkpoint has no memory image
const ShellNotEnabled = 0       // Shell is not enabled for this session

// SessionInfoDir is the fixed-path global store of SessionInfo records.
const SessionInfoDir = "/tmp/waypoint-sessions-info"

// Configuration defaults. loadConfig may override them from WAYPOINT_*
// environment variables or a config.json file.
var (
	// DefaultSessionsDir is the default directory for storing checkpoint sessions.
	DefaultSessionsDir = "/tmp/waypoint-sessions"
	// DefaultBashInitSrc is the default source path for the bash_init binary used for shell sessions.
	DefaultBashInitSrc = "./bash_init"
	// PreserveSessionOnCleanup skips final removal after cleanup unmounts and kills resources.
	PreserveSessionOnCleanup = false
)

type config struct {
	SessionsDir              string `json:"sessions_dir,omitempty"`
	BashInitSrc              string `json:"bash_init_src,omitempty"`
	PreserveSessionOnCleanup bool   `json:"preserve_session_on_cleanup,omitempty"`
}

// loadConfig applies configuration onto the defaults above. Per field, a
// WAYPOINT_* environment variable wins over the config file, which wins over
// the built-in default.
func loadConfig() {
	var cfg config
	if path := findConfigFile(); path != "" {
		if data, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(data, &cfg)
		}
	}

	if v := os.Getenv("WAYPOINT_SESSIONS_DIR"); v != "" {
		DefaultSessionsDir = v
	} else if cfg.SessionsDir != "" {
		DefaultSessionsDir = cfg.SessionsDir
	}
	if v := os.Getenv("WAYPOINT_BASH_INIT_SRC"); v != "" {
		DefaultBashInitSrc = v
	} else if cfg.BashInitSrc != "" {
		DefaultBashInitSrc = cfg.BashInitSrc
	}
	if v := os.Getenv("WAYPOINT_PRESERVE_SESSION_ON_CLEANUP"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			PreserveSessionOnCleanup = parsed
		}
	} else {
		PreserveSessionOnCleanup = cfg.PreserveSessionOnCleanup
	}
}

// findConfigFile returns the config file path by precedence: explicit
// WAYPOINT_CONFIG, config.json next to the executable, user config
// ($XDG_CONFIG_HOME/waypoint/config.json or ~/.waypoint/config.json),
// then /etc/waypoint/config.json.
func findConfigFile() string {
	if p := os.Getenv("WAYPOINT_CONFIG"); p != "" {
		return p
	}

	var candidates []string
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "config.json"))
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		candidates = append(candidates, filepath.Join(xdg, "waypoint", "config.json"))
	} else if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".waypoint", "config.json"))
	}
	candidates = append(candidates, "/etc/waypoint/config.json")

	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// NewManagerWithSession creates a new manager with a random session ID.
func NewManagerWithSession() (*Manager, string, error) {
	sessionID, err := generateSessionID()
	if err != nil {
		return nil, "", fmt.Errorf("failed to generate session ID: %w", err)
	}

	loadConfig()

	manager := newManager(filepath.Join(DefaultSessionsDir, sessionID))
	manager.sessionID = sessionID
	manager.shellPid = ShellNotEnabled

	if err := saveSessionInfo(sessionID, manager); err != nil {
		return nil, "", fmt.Errorf("failed to save session info: %w", err)
	}

	return manager, sessionID, nil
}

// LoadManager loads an existing manager by session ID.
func LoadManager(sessionID string) (*Manager, error) {
	sessionInfo, err := loadSessionInfo(sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to load session: %w", err)
	}

	manager := newManager(sessionInfo.BaseDir)
	manager.sessionID = sessionID
	manager.originalDir = sessionInfo.OriginalDir
	manager.workOverlay = sessionInfo.WorkOverlay
	manager.shellPid = sessionInfo.ShellPid
	manager.shellSocket = sessionInfo.ShellSocket

	return manager, nil
}

// newManager scaffolds the session directory tree and returns the handle.
func newManager(baseDir string) *Manager {
	metadataDir := filepath.Join(baseDir, "metadata")
	workOverlay := filepath.Join(baseDir, "work")

	os.MkdirAll(metadataDir, 0755)
	os.MkdirAll(workOverlay, 0755)
	os.MkdirAll(filepath.Join(baseDir, "temp"), 0777)
	os.MkdirAll(filepath.Join(baseDir, "checkpoints"), 0755)
	os.MkdirAll(filepath.Join(baseDir, "forks"), 0755)
	os.MkdirAll(filepath.Join(baseDir, "locks"), 0755)

	return &Manager{
		baseDir:     baseDir,
		metadataDir: metadataDir,
		workOverlay: workOverlay,
	}
}

func generateSessionID() (string, error) {
	bytes := make([]byte, 8)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// --- global session registry ---

// saveSessionInfo converts the Manager to a SessionInfo record and saves it
// to the fixed-path global store.
func saveSessionInfo(sessionID string, manager *Manager) error {
	os.MkdirAll(SessionInfoDir, 0755)

	sessionInfo := SessionInfo{
		SessionID:   sessionID,
		BaseDir:     manager.baseDir,
		OriginalDir: manager.originalDir,
		WorkOverlay: manager.workOverlay,
		CreatedAt:   time.Now().Unix(),
		ShellPid:    manager.shellPid,
		ShellSocket: manager.shellSocket,
	}
	return writeSessionInfo(&sessionInfo)
}

func loadSessionInfo(sessionID string) (*SessionInfo, error) {
	data, err := os.ReadFile(filepath.Join(SessionInfoDir, sessionID+".json"))
	if err != nil {
		return nil, fmt.Errorf("session not found: %w", err)
	}

	var sessionInfo SessionInfo
	err = json.Unmarshal(data, &sessionInfo)
	return &sessionInfo, err
}

func removeSessionInfo(sessionID string) error {
	return os.Remove(filepath.Join(SessionInfoDir, sessionID+".json"))
}

func updateSessionEnvironment(sessionID, originalDir, workOverlay string) error {
	sessionInfo, err := loadSessionInfo(sessionID)
	if err != nil {
		return err
	}
	sessionInfo.OriginalDir = originalDir
	sessionInfo.WorkOverlay = workOverlay
	return writeSessionInfo(sessionInfo)
}

func writeSessionInfo(sessionInfo *SessionInfo) error {
	data, err := json.MarshalIndent(sessionInfo, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(SessionInfoDir, sessionInfo.SessionID+".json"), data, 0o644)
}

// --- on-disk layout ---

func (m *Manager) checkpointDir(checkpointID string) string {
	return filepath.Join(m.baseDir, "checkpoints", checkpointID)
}

func (m *Manager) checkpointUpperDir(checkpointID string) string {
	return filepath.Join(m.checkpointDir(checkpointID), "upper")
}

func (m *Manager) checkpointCriuDir(checkpointID string) string {
	return filepath.Join(m.checkpointDir(checkpointID), "criu")
}

func (m *Manager) forksDir() string {
	return filepath.Join(m.baseDir, "forks")
}

func (m *Manager) forkDir(forkID string) string {
	return filepath.Join(m.forksDir(), forkID)
}

func (m *Manager) sessionLockPath() string {
	return filepath.Join(m.baseDir, "locks", "session.lock")
}

func (m *Manager) forkLockPath(forkID string) string {
	return filepath.Join(m.forkDir(forkID), "lock")
}

// canonicalSocketPath is the shell socket path as seen from inside the
// session overlay; it is baked into CRIU images, so every fork of this
// session shares it.
func (m *Manager) canonicalSocketPath() string {
	return filepath.Join(m.baseDir, "temp", fmt.Sprintf("shell_%s.sock", m.sessionID))
}

func (m *Manager) shellLogPath() string {
	return filepath.Join(m.baseDir, "temp", fmt.Sprintf("shell_%s.log", m.sessionID))
}

// --- cross-process locking & atomic writes ---

// withFileLock runs fn while holding an exclusive flock on path. Locks are
// file-based because each CLI invocation is a separate process.
func withFileLock(path string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock %s failed: %w", path, err)
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)

	return fn()
}

func (m *Manager) withSessionLock(fn func() error) error {
	return withFileLock(m.sessionLockPath(), fn)
}

func (m *Manager) withForkLock(forkID string, fn func() error) error {
	return withFileLock(m.forkLockPath(forkID), fn)
}

// atomicWriteFile writes data to path via a temp file + rename so readers
// never observe a torn write.
func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// --- listing ---

// SessionListing is the stable JSON shape emitted by `waypoint list --json`.
// Checkpoints are the immutable DAG nodes; Forks are the live instances.
type SessionListing struct {
	SessionID   string     `json:"session_id"`
	Checkpoints []Metadata `json:"checkpoints"`
	Forks       []*Fork    `json:"forks"`
}

// ListSession collects checkpoint metadata and live fork records.
func (m *Manager) ListSession() (*SessionListing, error) {
	ids, err := m.ListCheckpoints()
	if err != nil {
		return nil, err
	}
	sort.Strings(ids)

	listing := &SessionListing{
		SessionID:   m.sessionID,
		Checkpoints: make([]Metadata, 0, len(ids)),
	}
	for _, id := range ids {
		metadata, err := m.loadMetadata(id)
		if err != nil {
			continue // e.g. torn write during concurrent snapshot; skip
		}
		listing.Checkpoints = append(listing.Checkpoints, *metadata)
	}

	forks, err := m.ListForks()
	if err != nil {
		return nil, err
	}
	sort.Slice(forks, func(i, j int) bool { return forks[i].ID < forks[j].ID })
	listing.Forks = forks
	return listing, nil
}

// ListCheckpoints returns the IDs of all checkpoints in this session.
func (m *Manager) ListCheckpoints() ([]string, error) {
	files, err := os.ReadDir(m.metadataDir)
	if err != nil {
		return nil, err
	}

	var checkpoints []string
	for _, file := range files {
		if strings.HasSuffix(file.Name(), ".json") && file.Name() != "environment.json" {
			checkpoints = append(checkpoints, strings.TrimSuffix(file.Name(), ".json"))
		}
	}
	return checkpoints, nil
}
