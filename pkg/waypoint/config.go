package waypoint

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Configuration variables and where each was resolved from ("default",
// "env:<VAR>", or "config:<path>"). loadConfig may override them.
var (
	// SessionInfoDir is the global store of SessionInfo records. The default
	// lives in /tmp, which on many distros is a tmpfs aged by
	// systemd-tmpfiles — sessions meant to survive a reboot (suspend/resume)
	// need this and SessionsDir on durable storage.
	SessionInfoDir       = "/tmp/waypoint-sessions-info"
	SessionInfoDirConfig = "default"

	// DefaultSessionsDir is the default directory for storing checkpoint sessions.
	DefaultSessionsDir       = "/tmp/waypoint-sessions"
	DefaultSessionsDirConfig = "default"

	// DefaultBashInitSrc is the default source path for the bash_init binary used for shell sessions.
	DefaultBashInitSrc       = "./bash_init"
	DefaultBashInitSrcConfig = "default"

	// PreserveSessionOnCleanup skips final removal after cleanup unmounts and kills resources.
	PreserveSessionOnCleanup       = false
	PreserveSessionOnCleanupConfig = "default"

	// TmpfsImages makes checkpoint dumps write their CRIU images to a tmpfs
	// dir (fast, no writeback storm) with an async flush to the durable
	// checkpoint dir; checkpoints/<ckpt>/criu is a symlink either way, so
	// all consumers see one stable path. Until a checkpoint's flush
	// completes, a host reboot loses its images.
	TmpfsImages       = false
	TmpfsImagesConfig = "default"

	// TmpfsImagesDir is the tmpfs root holding per-session image dirs.
	TmpfsImagesDir       = "/dev/shm/waypoint"
	TmpfsImagesDirConfig = "default"

	// PhaseStats enables phase-level latency instrumentation: each fork
	// restore and checkpoint assembles a timing breakdown (from CRIU's
	// stats-dump / stats-restore images and our own wall clocks), persists
	// it in fork.json / checkpoint metadata, and the CLI prints it as flat
	// key_ms= tokens. Off by default; when on it adds a stats-image parse to
	// every fork and snapshot.
	PhaseStats       = false
	PhaseStatsConfig = "default"
)

type config struct {
	SessionInfoDir           string `json:"session_info_dir,omitempty"`
	SessionsDir              string `json:"sessions_dir,omitempty"`
	BashInitSrc              string `json:"bash_init_src,omitempty"`
	PreserveSessionOnCleanup *bool  `json:"preserve_session_on_cleanup,omitempty"`
	TmpfsImages              *bool  `json:"tmpfs_images,omitempty"`
	TmpfsImagesDir           string `json:"tmpfs_images_dir,omitempty"`
	PhaseStats               *bool  `json:"phase_stats,omitempty"`
}

// ConfigValue reports an effective configuration value and where it came from.
type ConfigValue struct {
	Value  any    `json:"value"`
	Source string `json:"source"`
}

// ConfigInfo reports the effective Waypoint configuration.
type ConfigInfo struct {
	SessionInfoDir           ConfigValue `json:"session_info_dir"`
	SessionsDir              ConfigValue `json:"sessions_dir"`
	BashInitSrc              ConfigValue `json:"bash_init_src"`
	PreserveSessionOnCleanup ConfigValue `json:"preserve_session_on_cleanup"`
	TmpfsImages              ConfigValue `json:"tmpfs_images"`
	TmpfsImagesDir           ConfigValue `json:"tmpfs_images_dir"`
	PhaseStats               ConfigValue `json:"phase_stats"`
}

// LoadConfigInfo loads custom configuration and returns the effective values.
func LoadConfigInfo() ConfigInfo {
	loadConfig()
	return ConfigInfo{
		SessionInfoDir: ConfigValue{
			Value:  SessionInfoDir,
			Source: SessionInfoDirConfig,
		},
		SessionsDir: ConfigValue{
			Value:  DefaultSessionsDir,
			Source: DefaultSessionsDirConfig,
		},
		BashInitSrc: ConfigValue{
			Value:  DefaultBashInitSrc,
			Source: DefaultBashInitSrcConfig,
		},
		PreserveSessionOnCleanup: ConfigValue{
			Value:  PreserveSessionOnCleanup,
			Source: PreserveSessionOnCleanupConfig,
		},
		TmpfsImages: ConfigValue{
			Value:  TmpfsImages,
			Source: TmpfsImagesConfig,
		},
		TmpfsImagesDir: ConfigValue{
			Value:  TmpfsImagesDir,
			Source: TmpfsImagesDirConfig,
		},
		PhaseStats: ConfigValue{
			Value:  PhaseStats,
			Source: PhaseStatsConfig,
		},
	}
}

// maxUnixSocketPath is the longest dialable unix socket path: sun_path holds
// 108 bytes including the trailing NUL.
const maxUnixSocketPath = 107

// validateSessionsDir fails fast when the sessions dir is so deep that the
// host-side shell socket dial path would exceed the unix sun_path limit.
// The host dials /proc/<pid>/root/<sessionsDir>/<16hex>/temp/shell_<16hex>.sock
// (see socketPathThroughProcRoot); past the limit the dial fails much later
// with an opaque "connect: invalid argument".
func validateSessionsDir(dir string) error {
	// Session paths become OverlayFS lowerdir/upperdir mount options, whose
	// syntax cannot escape these separators.
	if strings.ContainsAny(dir, ":,") {
		return fmt.Errorf("sessions dir %s contains ':' or ',' — not representable in OverlayFS mount options; use a different sessions dir (WAYPOINT_SESSIONS_DIR or sessions_dir in config.json)", dir)
	}
	procRootPrefix := len("/proc/1234567/root") // worst-case 7-digit pid
	sessionSuffix := len("/0123456789abcdef/temp/shell_0123456789abcdef.sock")
	worstCase := procRootPrefix + len(filepath.Clean(dir)) + sessionSuffix
	if worstCase > maxUnixSocketPath {
		return fmt.Errorf("sessions dir %s is too deep — worst-case socket path %d chars > %d (unix sun_path limit); use a shorter sessions dir (WAYPOINT_SESSIONS_DIR or sessions_dir in config.json)",
			dir, worstCase, maxUnixSocketPath)
	}
	return nil
}

// boolEnvOverride returns a parsed boolean environment override. Invalid
// non-empty values are warned about and treated as unset so they do not hide
// a valid value from the config file.
func boolEnvOverride(name string) (bool, bool) {
	raw := os.Getenv(name)
	if raw == "" {
		return false, false
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: ignoring invalid boolean %s=%q; using config/default instead\n", name, raw)
		return false, false
	}
	return value, true
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
	if v := os.Getenv("WAYPOINT_SESSION_INFO_DIR"); v != "" {
		SessionInfoDir = v
		SessionInfoDirConfig = "env:WAYPOINT_SESSION_INFO_DIR"
	}
	if v := os.Getenv("WAYPOINT_SESSIONS_DIR"); v != "" {
		DefaultSessionsDir = v
		DefaultSessionsDirConfig = "env:WAYPOINT_SESSIONS_DIR"
	}
	if v := os.Getenv("WAYPOINT_BASH_INIT_SRC"); v != "" {
		DefaultBashInitSrc = v
		DefaultBashInitSrcConfig = "env:WAYPOINT_BASH_INIT_SRC"
	}
	preserveEnv, preserveEnvSet := boolEnvOverride("WAYPOINT_PRESERVE_SESSION_ON_CLEANUP")
	if preserveEnvSet {
		PreserveSessionOnCleanup = preserveEnv
		PreserveSessionOnCleanupConfig = "env:WAYPOINT_PRESERVE_SESSION_ON_CLEANUP"
	}
	tmpfsEnv, tmpfsEnvSet := boolEnvOverride("WAYPOINT_TMPFS_IMAGES")
	if tmpfsEnvSet {
		TmpfsImages = tmpfsEnv
		TmpfsImagesConfig = "env:WAYPOINT_TMPFS_IMAGES"
	}
	if v := os.Getenv("WAYPOINT_TMPFS_DIR"); v != "" {
		TmpfsImagesDir = v
		TmpfsImagesDirConfig = "env:WAYPOINT_TMPFS_DIR"
	}
	phaseStatsEnv, phaseStatsEnvSet := boolEnvOverride("WAYPOINT_PHASE_STATS")
	if phaseStatsEnvSet {
		PhaseStats = phaseStatsEnv
		PhaseStatsConfig = "env:WAYPOINT_PHASE_STATS"
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
				configSource := "config:" + cfgPath
				if cfg.SessionInfoDir != "" && os.Getenv("WAYPOINT_SESSION_INFO_DIR") == "" {
					SessionInfoDir = cfg.SessionInfoDir
					SessionInfoDirConfig = configSource
				}
				if cfg.SessionsDir != "" && os.Getenv("WAYPOINT_SESSIONS_DIR") == "" {
					DefaultSessionsDir = cfg.SessionsDir
					DefaultSessionsDirConfig = configSource
				}
				if cfg.BashInitSrc != "" && os.Getenv("WAYPOINT_BASH_INIT_SRC") == "" {
					DefaultBashInitSrc = cfg.BashInitSrc
					DefaultBashInitSrcConfig = configSource
				}
				if cfg.PreserveSessionOnCleanup != nil && !preserveEnvSet {
					PreserveSessionOnCleanup = *cfg.PreserveSessionOnCleanup
					PreserveSessionOnCleanupConfig = configSource
				}
				if cfg.TmpfsImages != nil && !tmpfsEnvSet {
					TmpfsImages = *cfg.TmpfsImages
					TmpfsImagesConfig = configSource
				}
				if cfg.TmpfsImagesDir != "" && os.Getenv("WAYPOINT_TMPFS_DIR") == "" {
					TmpfsImagesDir = cfg.TmpfsImagesDir
					TmpfsImagesDirConfig = configSource
				}
				if cfg.PhaseStats != nil && !phaseStatsEnvSet {
					PhaseStats = *cfg.PhaseStats
					PhaseStatsConfig = configSource
				}
			}
		}
	}
}
