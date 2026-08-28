package waypoint

// All structs, constants, and interfaces

// Manager manages runtime checkpoint sessions, the main struct.
type Manager struct {
	baseDir       string   // Base directory for this session, e.g., /tmp/waypoint-sessions/a1b2c3d4e5f6g7h8
	metadataDir   string   // Directory for metadata files, e.g., /tmp/waypoint-sessions/a1b2c3d4e5f6g7h8/metadata
	workOverlay   string   // Current working overlay mount point, e.g., /tmp/waypoint-sessions/a1b2c3d4e5f6g7h8/work
	originalDir   string   // Original directory being managed, e.g., /home/user/app-data
	sessionID     string   // Unique session identifier, e.g., a1b2c3d4e5f6g7h8
	shellPid      int      // PID of the shell process if a shell is enabled, ShellNotEnabled(=0) otherwise
	shellSocket   string   // Path to the shell socket if enabled, empty otherwise
	currentParent []string // Current parent checkpoints
	imageEnv      []string // Env from the built image's config, empty for non-build sessions
	imageWorkDir  string   // WorkingDir from the built image's config, empty if unset
}

// Metadata represents the metadata stored for each checkpoint.
// It is serialized to JSON and stored in the per-session metadata directory for snapshot tracking.
type Metadata struct {
	ID          string   `json:"id"`
	PID         int      `json:"pid"`
	Timestamp   int64    `json:"timestamp"`
	OriginalDir string   `json:"original_dir"`
	SessionID   string   `json:"session_id"`
	ParentList  []string `json:"parent_list,omitempty"`
}

// SessionInfo holds information about a checkpoint session.
// It is serialized to JSON and stored in a globally known location for session tracking.
type SessionInfo struct {
	SessionID     string   `json:"session_id"`
	BaseDir       string   `json:"base_dir"`
	OriginalDir   string   `json:"original_dir"`
	WorkOverlay   string   `json:"work_overlay"`
	CreatedAt     int64    `json:"created_at"`
	CurrentParent []string `json:"current_parent"`
	ShellPid      int      `json:"shell_pid"`
	ShellSocket   string   `json:"shell_socket,omitempty"`
	ImageEnv      []string `json:"image_env,omitempty"`
	ImageWorkDir  string   `json:"image_workdir,omitempty"`
}

// ImageConfig holds the image settings Waypoint applies to a session.
type ImageConfig struct {
	Env        []string
	WorkingDir string
}

// PID values for special cases

const SkipMemoryCheckpoint = -1 // User requested to skip memory checkpoint
const ShellNotEnabled = 0       // Shell is not enabled for this session
const PidNotProvided = -2       // PID not provided for checkpointing

const SessionInfoDir = "/tmp/waypoint-sessions-info"

// FallbackPath is used when the image config carries no PATH of its own.
const FallbackPath = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// The below section declares configuration variables.
// Those variables can be overridden by configuration.

// DefaultSessionsDir is the default directory for storing checkpoint sessions.
var DefaultSessionsDir = "/tmp/waypoint-sessions"

// DefaultSessionsDirConfig reports where DefaultSessionsDir was configured from.
var DefaultSessionsDirConfig = "default"

// DefaultBashInitSrc is the default source path for the bash_init binary used for shell sessions.
var DefaultBashInitSrc = "./bash_init"

// DefaultBashInitSrcConfig reports where DefaultBashInitSrc was configured from.
var DefaultBashInitSrcConfig = "default"

// PreserveSessionOnCleanup skips final removal after cleanup unmounts and kills resources.
var PreserveSessionOnCleanup = false

// PreserveSessionOnCleanupConfig reports where PreserveSessionOnCleanup was configured from.
var PreserveSessionOnCleanupConfig = "default"
