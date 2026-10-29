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

	// IncrementalDumps makes each dump write only the pages a fork changed
	// since its last restore, chaining its images to the restored ones via
	// CRIU's parent links. Needs a CRIU with restore --next-images-dir; has
	// no effect while TmpfsImages is on (see incrementalDumps).
	IncrementalDumps       = false
	IncrementalDumpsConfig = "default"

	// PhaseStats enables phase-level latency instrumentation: each fork
	// restore and checkpoint assembles a timing breakdown (from CRIU's
	// stats-dump / stats-restore images and our own wall clocks), persists
	// it in fork.json / checkpoint metadata, and the CLI prints it as flat
	// key_ms= tokens. Off by default; when on it adds a stats-image parse to
	// every fork and snapshot.
	PhaseStats       = false
	PhaseStatsConfig = "default"

	// LogLevel is Waypoint's log level. For now it is the -v level of every
	// criu dump and restore, written to dump.log in the images dir and
	// restore.log in the fork dir: 0 none, 1 errors, 2 warnings, 3 info,
	// 4 debug. Levels 3 and 4 log a line per piece of memory read at every
	// level of a parent chain (and 4 a line per page range an incremental
	// dump checks), which on a deep chain of scattered writes takes longer
	// than the restore itself.
	LogLevel       = 2
	LogLevelConfig = "default"

	// NetworkMode is the network mode of new sessions; a session keeps the
	// mode it was created with.
	NetworkMode       = NetworkShared
	NetworkModeConfig = "default"
)

// Network modes: forks run in the host network namespace, or each in its own
// with outbound access through pasta (netns.go).
const (
	NetworkShared  = "shared"
	NetworkPrivate = "private"
)

func validNetworkMode(mode string) bool {
	return mode == NetworkShared || mode == NetworkPrivate
}

type config struct {
	SessionInfoDir           string `json:"session_info_dir,omitempty"`
	SessionsDir              string `json:"sessions_dir,omitempty"`
	BashInitSrc              string `json:"bash_init_src,omitempty"`
	PreserveSessionOnCleanup *bool  `json:"preserve_session_on_cleanup,omitempty"`
	TmpfsImages              *bool  `json:"tmpfs_images,omitempty"`
	TmpfsImagesDir           string `json:"tmpfs_images_dir,omitempty"`
	IncrementalDumps         *bool  `json:"incremental_dumps,omitempty"`
	PhaseStats               *bool  `json:"phase_stats,omitempty"`
	LogLevel                 *int   `json:"waypoint_log_level,omitempty"`
	Network                  string `json:"network,omitempty"`
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
	IncrementalDumps         ConfigValue `json:"incremental_dumps"`
	PhaseStats               ConfigValue `json:"phase_stats"`
	LogLevel                 ConfigValue `json:"waypoint_log_level"`
	Network                  ConfigValue `json:"network"`
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
		IncrementalDumps: incrementalDumpsInfo(),
		PhaseStats: ConfigValue{
			Value:  PhaseStats,
			Source: PhaseStatsConfig,
		},
		LogLevel: ConfigValue{
			Value:  LogLevel,
			Source: LogLevelConfig,
		},
		Network: ConfigValue{
			Value:  NetworkMode,
			Source: NetworkModeConfig,
		},
	}
}

// incrementalDumps reports whether dumps and restores use parent image chains.
// tmpfs images move after the restore, which would leave the chain's links
// dangling, so the two modes exclude each other.
func incrementalDumps() bool { return IncrementalDumps && !TmpfsImages }

// incrementalDumpsInfo reports the effective value, noting when tmpfs_images
// overrides a configured true.
func incrementalDumpsInfo() ConfigValue {
	source := IncrementalDumpsConfig
	if IncrementalDumps && !incrementalDumps() {
		source += " (off: tmpfs_images)"
	}
	return ConfigValue{Value: incrementalDumps(), Source: source}
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

// validLogLevel reports whether level is one of criu's -v levels.
func validLogLevel(level int) bool { return level >= 0 && level <= 4 }

// logLevelEnvOverride returns a parsed WAYPOINT_LOG_LEVEL. Invalid
// non-empty values are warned about and treated as unset, as in
// boolEnvOverride.
func logLevelEnvOverride() (int, bool) {
	raw := os.Getenv("WAYPOINT_LOG_LEVEL")
	if raw == "" {
		return 0, false
	}
	level, err := strconv.Atoi(raw)
	if err != nil || !validLogLevel(level) {
		fmt.Fprintf(os.Stderr, "warning: ignoring invalid WAYPOINT_LOG_LEVEL=%q (want 0 to 4); using config/default instead\n", raw)
		return 0, false
	}
	return level, true
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
	incrementalEnv, incrementalEnvSet := boolEnvOverride("WAYPOINT_INCREMENTAL_DUMPS")
	if incrementalEnvSet {
		IncrementalDumps = incrementalEnv
		IncrementalDumpsConfig = "env:WAYPOINT_INCREMENTAL_DUMPS"
	}
	phaseStatsEnv, phaseStatsEnvSet := boolEnvOverride("WAYPOINT_PHASE_STATS")
	if phaseStatsEnvSet {
		PhaseStats = phaseStatsEnv
		PhaseStatsConfig = "env:WAYPOINT_PHASE_STATS"
	}
	logLevelEnv, logLevelEnvSet := logLevelEnvOverride()
	if logLevelEnvSet {
		LogLevel = logLevelEnv
		LogLevelConfig = "env:WAYPOINT_LOG_LEVEL"
	}
	networkEnvSet := false
	if v := os.Getenv("WAYPOINT_NETWORK"); v != "" {
		if validNetworkMode(v) {
			NetworkMode = v
			NetworkModeConfig = "env:WAYPOINT_NETWORK"
			networkEnvSet = true
		} else {
			fmt.Fprintf(os.Stderr, "warning: ignoring invalid WAYPOINT_NETWORK=%q (want %q or %q); using config/default instead\n", v, NetworkShared, NetworkPrivate)
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
				if cfg.IncrementalDumps != nil && !incrementalEnvSet {
					IncrementalDumps = *cfg.IncrementalDumps
					IncrementalDumpsConfig = configSource
				}
				if cfg.PhaseStats != nil && !phaseStatsEnvSet {
					PhaseStats = *cfg.PhaseStats
					PhaseStatsConfig = configSource
				}
				if cfg.LogLevel != nil && !logLevelEnvSet {
					if validLogLevel(*cfg.LogLevel) {
						LogLevel = *cfg.LogLevel
						LogLevelConfig = configSource
					} else {
						fmt.Fprintf(os.Stderr, "warning: ignoring invalid waypoint_log_level %d in %s (want 0 to 4)\n", *cfg.LogLevel, cfgPath)
					}
				}
				if cfg.Network != "" && !networkEnvSet {
					if validNetworkMode(cfg.Network) {
						NetworkMode = cfg.Network
						NetworkModeConfig = configSource
					} else {
						fmt.Fprintf(os.Stderr, "warning: ignoring invalid network %q in %s (want %q or %q)\n", cfg.Network, cfgPath, NetworkShared, NetworkPrivate)
					}
				}
			}
		}
	}
}
