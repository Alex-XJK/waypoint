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
	// shellStartTime is shellPid's /proc stat starttime, recorded at spawn
	// so kills can verify the identity (PID-reuse safety). 0 = unknown.
	shellStartTime uint64
	// imageEnv and imageWorkDir carry the built image's OCI config into the
	// session shell, so a task's `ENV` and `WORKDIR` mean the same thing here
	// as they do under docker. Empty for `init` sessions, which have no image.
	imageEnv     []string
	imageWorkDir string
	// network is the session's network mode, set at creation; empty means
	// NetworkShared. networkLock is its criu --network-lock method.
	network     string
	networkLock string
	// group and service, set by SetGroup, record the session's owner.
	group, service string
	// mainAttach, set by AttachMain, connects main to a fabric at StartShell.
	mainAttach *NetworkAttachment
	// guest, set by SetGuestOptions, customizes the guest at StartShell.
	guest GuestOptions
	// imageEntrypoint and imageCmd are a built image's default command.
	imageEntrypoint []string
	imageCmd        []string
}

// SessionInfo holds information about a checkpoint session.
// It is serialized to JSON and stored in a globally known location for session tracking.
type SessionInfo struct {
	SessionID      string   `json:"session_id"`
	BaseDir        string   `json:"base_dir"`
	OriginalDir    string   `json:"original_dir"`
	WorkOverlay    string   `json:"work_overlay"`
	CreatedAt      int64    `json:"created_at"`
	ShellPid       int      `json:"shell_pid"`
	ShellStartTime uint64   `json:"shell_start_time,omitempty"`
	ShellSocket    string   `json:"shell_socket,omitempty"`
	ImageEnv       []string `json:"image_env,omitempty"`
	ImageWorkDir   string   `json:"image_workdir,omitempty"`
	Network        string   `json:"network,omitempty"`
	NetworkLock    string   `json:"network_lock,omitempty"`
	// Group and Service name the compose project and service that own the
	// session, if any (see pkg/group).
	Group   string `json:"group,omitempty"`
	Service string `json:"service,omitempty"`
}

// PID values for special cases
const SkipMemoryCheckpoint = -1 // Checkpoint has no memory image
const ShellNotEnabled = 0       // Shell is not enabled for this session

// Configuration lives in config.go (SessionInfoDir, DefaultSessionsDir,
// DefaultBashInitSrc, PreserveSessionOnCleanup, and loadConfig).

// NewManagerWithSession creates a new manager with a random session ID.
func NewManagerWithSession() (*Manager, string, error) {
	return NewManagerWithNetwork("")
}

// NewManagerWithNetwork is NewManagerWithSession with the session's network
// mode set to network instead of the configured one ("" keeps it).
func NewManagerWithNetwork(network string) (*Manager, string, error) {
	if network != "" && !validNetworkMode(network) {
		return nil, "", fmt.Errorf("invalid network mode %q", network)
	}
	sessionID, err := generateSessionID()
	if err != nil {
		return nil, "", fmt.Errorf("failed to generate session ID: %w", err)
	}

	loadConfig()
	if network == "" {
		network = NetworkMode
	}

	if err := validateSessionsDir(DefaultSessionsDir); err != nil {
		return nil, "", err
	}

	manager := newManager(filepath.Join(DefaultSessionsDir, sessionID))
	manager.sessionID = sessionID
	manager.shellPid = ShellNotEnabled
	manager.network = network
	if manager.privateNetwork() {
		manager.networkLock = detectNetworkLock()
	}

	if err := saveSessionInfo(sessionID, manager); err != nil {
		return nil, "", fmt.Errorf("failed to save session info: %w", err)
	}

	return manager, sessionID, nil
}

// LoadManager loads an existing manager by session ID.
func LoadManager(sessionID string) (*Manager, error) {
	loadConfig() // session paths come from the registry; this resolves behavior flags (e.g. TmpfsImages)

	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}
	sessionInfo, err := loadSessionInfo(sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to load session: %w", err)
	}

	manager := newManager(sessionInfo.BaseDir)
	manager.sessionID = sessionID
	manager.originalDir = sessionInfo.OriginalDir
	manager.workOverlay = sessionInfo.WorkOverlay
	manager.shellPid = sessionInfo.ShellPid
	manager.shellStartTime = sessionInfo.ShellStartTime
	manager.shellSocket = sessionInfo.ShellSocket
	manager.imageEnv = sessionInfo.ImageEnv
	manager.imageWorkDir = sessionInfo.ImageWorkDir
	manager.network = sessionInfo.Network
	manager.networkLock = sessionInfo.NetworkLock
	manager.group, manager.service = sessionInfo.Group, sessionInfo.Service

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
		SessionID:      sessionID,
		BaseDir:        manager.baseDir,
		OriginalDir:    manager.originalDir,
		WorkOverlay:    manager.workOverlay,
		CreatedAt:      time.Now().Unix(),
		ShellPid:       manager.shellPid,
		ShellStartTime: manager.shellStartTime,
		ShellSocket:    manager.shellSocket,
		ImageEnv:       manager.imageEnv,
		ImageWorkDir:   manager.imageWorkDir,
		Network:        manager.network,
		NetworkLock:    manager.networkLock,
		Group:          manager.group,
		Service:        manager.service,
	}
	return writeSessionInfo(&sessionInfo)
}

// SetGroup records that the session is service of compose project group.
func (m *Manager) SetGroup(group, service string) error {
	m.group, m.service = group, service
	return saveSessionInfo(m.sessionID, m)
}

// Group returns the compose project and service recorded by SetGroup; both
// are empty for a standalone session.
func (m *Manager) Group() (group, service string) {
	return m.group, m.service
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

// LoadSessionInfo returns persisted information for a checkpoint session.
func LoadSessionInfo(sessionID string) (*SessionInfo, error) {
	loadConfig() // SessionInfoDir is configurable
	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}
	return loadSessionInfo(sessionID)
}

// ListSessions returns all session IDs recorded in the global session store.
func ListSessions() ([]string, error) {
	loadConfig() // SessionInfoDir is configurable
	files, err := os.ReadDir(SessionInfoDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}

	var sessions []string
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		sessions = append(sessions, strings.TrimSuffix(file.Name(), ".json"))
	}
	sort.Strings(sessions)
	return sessions, nil
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

// --- identifier validation ---

// maxIDLength bounds session, checkpoint, and fork identifiers. They become
// path components under the session tree, so this keeps the resulting paths
// (in particular the canonical socket path, see validateSessionsDir) well
// inside the limits the rest of the system assumes.
const maxIDLength = 64

// validateID checks a user-supplied identifier before it is used to build a
// path or a mount option. Identifiers are single path components under the
// session tree and are spliced into OverlayFS lowerdir/upperdir options, so
// they may contain neither path separators nor the option separators ':'
// and ',' — the same constraint validateSessionsDir enforces for the
// sessions directory. Requiring a leading letter or digit additionally rules
// out "." and ".." (path traversal) and leading dashes (flag lookalikes).
func validateID(kind, id string) error {
	if id == "" {
		return fmt.Errorf("%s ID must not be empty", kind)
	}
	if len(id) > maxIDLength {
		return fmt.Errorf("invalid %s ID %q: %d bytes exceeds the %d-byte limit", kind, id, len(id), maxIDLength)
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		if i > 0 && (c == '.' || c == '_' || c == '-') {
			continue
		}
		return fmt.Errorf("invalid %s ID %q: must start with a letter or digit and may otherwise contain only letters, digits, '.', '_' and '-'", kind, id)
	}
	return nil
}

// ValidateID checks an identifier of the given kind (for messages) against
// the rules for session, fork, and checkpoint IDs.
func ValidateID(kind, id string) error { return validateID(kind, id) }

func validateSessionID(id string) error { return validateID("session", id) }
func validateForkID(id string) error    { return validateID("fork", id) }

// validateCheckpointID also rejects "current", which older releases used as a
// pseudo-ID for the live state.
func validateCheckpointID(id string) error {
	if id == "current" {
		return fmt.Errorf("invalid checkpoint ID: %q is reserved", id)
	}
	return validateID("checkpoint", id)
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
	// Fork roots can be removed while their locks are held. Keep lock paths in
	// the stable session namespace so their inodes cannot be replaced mid-lock.
	return filepath.Join(m.baseDir, "locks", "fork-"+forkID+".lock")
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

// WithFileLock is withFileLock, for coordinators that keep their own locks
// next to sessions' (see pkg/group).
func WithFileLock(path string, fn func() error) error { return withFileLock(path, fn) }

// WithSharedFileLock runs fn while holding a shared flock on path: any number
// of shared holders run together, and none while WithFileLock holds path.
func WithSharedFileLock(path string, fn func() error) error {
	return flockFile(path, unix.LOCK_SH, fn)
}

// AtomicWriteFile is atomicWriteFile, for the same callers.
func AtomicWriteFile(path string, data []byte, mode os.FileMode) error {
	return atomicWriteFile(path, data, mode)
}

// withFileLock runs fn while holding an exclusive flock on path. Locks are
// file-based because each CLI invocation is a separate process.
func withFileLock(path string, fn func() error) error {
	return flockFile(path, unix.LOCK_EX, fn)
}

func flockFile(path string, how int, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := unix.Flock(int(f.Fd()), how); err != nil {
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
