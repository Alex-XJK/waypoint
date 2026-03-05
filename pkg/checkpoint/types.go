package checkpoint

// All structs, constants, and interfaces

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Manager manages runtime checkpoint sessions, the main struct.
type Manager struct {
	baseDir       string   // Base directory for this session, e.g., /tmp/checkpoint-sessions/a1b2c3d4e5f6g7h8
	metadataDir   string   // Directory for metadata files, e.g., /tmp/checkpoint-sessions/a1b2c3d4e5f6g7h8/metadata
	workOverlay   string   // Current working overlay mount point, e.g., /tmp/checkpoint-sessions/a1b2c3d4e5f6g7h8/work
	originalDir   string   // Original directory being managed, e.g., /home/user/app-data
	currDir       string
	sessionID     string   // Unique session identifier, e.g., a1b2c3d4e5f6g7h8
	branchID      string   // Unique branch identifier 
	shellPid      int      // PID of the shell process if a shell is enabled, ShellNotEnabled(=0) otherwise
	shellSocket   string   // Path to the shell socket if enabled, empty otherwise
	shellRdev	  uint64   // pty slave rdev
	shellDev	  uint64   // pty slave dev
	currentParent []string // Current parent checkpoints
}

// Metadata represents the metadata stored for each checkpoint.
// It is serialized to JSON and stored in the per-session metadata directory for snapshot tracking.
// Add branchID? Not sure if necessary
type Metadata struct {
	ID          string   `json:"id"`
	PID         int      `json:"pid"`
	Timestamp   int64    `json:"timestamp"`
	OriginalDir string   `json:"original_dir"`
	SessionID   string   `json:"session_id"`
	Rdev		uint64   `json:"rdev"`
	Dev			uint64   `json:"dev"`
	ParentList  []string `json:"parent_list,omitempty"`
}

// SessionInfo holds information about a checkpoint session.
// It is serialized to JSON and stored in a globally known location for session tracking.
type SessionInfo struct {
	SessionID     string   `json:"session_id"`
	BranchID      string   `json:"branch_id"`
	BaseDir       string   `json:"base_dir"`
	OriginalDir   string   `json:"original_dir"`
	WorkOverlay   string   `json:"work_overlay"`
	CurrDir       string   `json:"curr_dir"`
	CreatedAt     int64    `json:"created_at"`
	CurrentParent []string `json:"current_parent"`
	ShellPid      int      `json:"shell_pid"`
	ShellSocket   string   `json:"shell_socket,omitempty"`
	ShellRdev	  uint64   `json:"shell_rdev,omitempty"`
	ShellDev	  uint64   `json:"shell_dev,omitempty"`
}

// PID values for special cases

const SkipMemoryCheckpoint = -1 // User requested to skip memory checkpoint
const ShellNotEnabled = 0       // Shell is not enabled for this session
const PidNotProvided = -2       // PID not provided for checkpointing

const SessionInfoDir = "/tmp/checkpoint-sessions-info"

const DefaultBranchID = "init_branch"

// The below section handles configuration loading.
// Those variables can be overridden by configuration.

// DefaultSessionsDir is the default directory for storing checkpoint sessions.
var DefaultSessionsDir = "/tmp/checkpoint-sessions"

// DefaultBashInitSrc is the default source path for the bash_init binary used for shell sessions.
var DefaultBashInitSrc = "./bash_init"

type config struct {
	SessionsDir string `json:"sessions_dir,omitempty"`
	BashInitSrc string `json:"bash_init_src,omitempty"`
}

// loadConfig loads custom configuration.
func loadConfig() {
	// Determine config by precedence:
	// 1) Direct environment variable of `CHECKPOINT_*` take the highest precedence.
	// 2) Determine config file path by precedence:
	//    a) explicit `CHECKPOINT_CONFIG` environment variable
	//    b) binary-side config: ./config.json (same dir as executable)
	//    c) user config: $XDG_CONFIG_HOME/checkpoint-lite/config.json or ~/.checkpoint-lite/config.json
	//    d) system config: /etc/checkpoint-lite/config.json
	// 3) If none found, keep defaults as set above.

	// 1) Direct env var overrides
	if v := os.Getenv("CHECKPOINT_SESSIONS_DIR"); v != "" {
		DefaultSessionsDir = v
	}
	if v := os.Getenv("CHECKPOINT_BASH_INIT_SRC"); v != "" {
		DefaultBashInitSrc = v
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
	cfgPath := os.Getenv("CHECKPOINT_CONFIG")

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
			p := filepath.Join(xdg, "checkpoint-lite", "config.json")
			if fileExists(p) {
				cfgPath = p
			}
		} else if home, err := os.UserHomeDir(); err == nil {
			p := filepath.Join(home, ".checkpoint-lite", "config.json")
			if fileExists(p) {
				cfgPath = p
			}
		}
	}

	if cfgPath == "" {
		// 2.d) system config
		p := filepath.Join("/etc", "checkpoint-lite", "config.json")
		if fileExists(p) {
			cfgPath = p
		}
	}

	if cfgPath != "" {
		if data, err := os.ReadFile(cfgPath); err == nil {
			var cfg config
			if err := json.Unmarshal(data, &cfg); err == nil {
				if cfg.SessionsDir != "" && os.Getenv("CHECKPOINT_SESSIONS_DIR") == "" {
					DefaultSessionsDir = cfg.SessionsDir
				}
				if cfg.BashInitSrc != "" && os.Getenv("CHECKPOINT_BASH_INIT_SRC") == "" {
					DefaultBashInitSrc = cfg.BashInitSrc
				}
			}
		}
	}
}
