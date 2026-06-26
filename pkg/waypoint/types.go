package waypoint

// All structs, constants, and interfaces

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
)

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
}

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
}

// PID values for special cases

const SkipMemoryCheckpoint = -1 // User requested to skip memory checkpoint
const ShellNotEnabled = 0       // Shell is not enabled for this session
const PidNotProvided = -2       // PID not provided for checkpointing

type CheckpointStatus string

const (
	CheckpointStatusCreating CheckpointStatus = "creating"
	CheckpointStatusReady    CheckpointStatus = "ready"
	CheckpointStatusFailed   CheckpointStatus = "failed"
)

const SessionInfoDir = "/tmp/waypoint-sessions-info"

// The below section handles configuration loading.
// Those variables can be overridden by configuration.

// DefaultSessionsDir is the default directory for storing checkpoint sessions.
var DefaultSessionsDir = "/tmp/waypoint-sessions"

// DefaultBashInitSrc is the default source path for the bash_init binary used for shell sessions.
var DefaultBashInitSrc = "./bash_init"

// PreserveSessionOnCleanup skips final removal after cleanup unmounts and kills resources.
var PreserveSessionOnCleanup = false

type config struct {
	SessionsDir              string `json:"sessions_dir,omitempty"`
	BashInitSrc              string `json:"bash_init_src,omitempty"`
	PreserveSessionOnCleanup bool   `json:"preserve_session_on_cleanup,omitempty"`
}

// loadConfig loads custom configuration.
func loadConfig() {
	// Determine config by precedence:
	// 1) Direct environment variable of `WAYPOINT_*` take the highest precedence.
	// 2) Determine config file path by precedence:
	//    a) explicit `WAYPOINT_CONFIG` environment variable
	//    b) binary-side config: ./config.json (same dir as executable)
	//    c) user config: $XDG_CONFIG_HOME/waypoint/config.json or ~/.waypoint/config.json
	//    d) system config: /etc/waypoint/config.json
	// 3) If none found, keep defaults as set above.

	// 1) Direct env var overrides
	if v := os.Getenv("WAYPOINT_SESSIONS_DIR"); v != "" {
		DefaultSessionsDir = v
	}
	if v := os.Getenv("WAYPOINT_BASH_INIT_SRC"); v != "" {
		DefaultBashInitSrc = v
	}
	if v := os.Getenv("WAYPOINT_PRESERVE_SESSION_ON_CLEANUP"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			PreserveSessionOnCleanup = parsed
		}
	}

	// 2) Config file path determination

	fileExists := func(path string) bool {
		if path == "" {
			return false
		}
		if _, err := os.Stat(path); err == nil {
			return true
		}
		return false
	}

	// 2.a) explicit env var
	cfgPath := os.Getenv("WAYPOINT_CONFIG")

	if cfgPath == "" {
		// 2.b) binary-side
		if exe, err := os.Executable(); err == nil {
			exeDir := filepath.Dir(exe)
			p := filepath.Join(exeDir, "config.json")
			if fileExists(p) {
				cfgPath = p
			}
		}
	}

	if cfgPath == "" {
		// 2.c) user config
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			p := filepath.Join(xdg, "waypoint", "config.json")
			if fileExists(p) {
				cfgPath = p
			}
		} else if home, err := os.UserHomeDir(); err == nil {
			p := filepath.Join(home, ".waypoint", "config.json")
			if fileExists(p) {
				cfgPath = p
			}
		}
	}

	if cfgPath == "" {
		// 2.d) system config
		p := filepath.Join("/etc", "waypoint", "config.json")
		if fileExists(p) {
			cfgPath = p
		}
	}

	if cfgPath != "" {
		if data, err := os.ReadFile(cfgPath); err == nil {
			var cfg config
			if err := json.Unmarshal(data, &cfg); err == nil {
				if cfg.SessionsDir != "" && os.Getenv("WAYPOINT_SESSIONS_DIR") == "" {
					DefaultSessionsDir = cfg.SessionsDir
				}
				if cfg.BashInitSrc != "" && os.Getenv("WAYPOINT_BASH_INIT_SRC") == "" {
					DefaultBashInitSrc = cfg.BashInitSrc
				}
				if os.Getenv("WAYPOINT_PRESERVE_SESSION_ON_CLEANUP") == "" {
					PreserveSessionOnCleanup = cfg.PreserveSessionOnCleanup
				}
			}
		}
	}
}
