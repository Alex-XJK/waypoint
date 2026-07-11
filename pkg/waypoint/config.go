package waypoint

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
)

type config struct {
	SessionsDir              string `json:"sessions_dir,omitempty"`
	BashInitSrc              string `json:"bash_init_src,omitempty"`
	PreserveSessionOnCleanup bool   `json:"preserve_session_on_cleanup,omitempty"`
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
}

// LoadConfigInfo loads custom configuration and returns the effective values.
func LoadConfigInfo() ConfigInfo {
	loadConfig()
	return ConfigInfo{
		SessionInfoDir: ConfigValue{
			Value:  SessionInfoDir,
			Source: "default",
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
	}
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
		DefaultSessionsDirConfig = "env:WAYPOINT_SESSIONS_DIR"
	}
	if v := os.Getenv("WAYPOINT_BASH_INIT_SRC"); v != "" {
		DefaultBashInitSrc = v
		DefaultBashInitSrcConfig = "env:WAYPOINT_BASH_INIT_SRC"
	}
	if v := os.Getenv("WAYPOINT_PRESERVE_SESSION_ON_CLEANUP"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			PreserveSessionOnCleanup = parsed
			PreserveSessionOnCleanupConfig = "env:WAYPOINT_PRESERVE_SESSION_ON_CLEANUP"
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
				if cfg.SessionsDir != "" && os.Getenv("WAYPOINT_SESSIONS_DIR") == "" {
					DefaultSessionsDir = cfg.SessionsDir
					DefaultSessionsDirConfig = configSource
				}
				if cfg.BashInitSrc != "" && os.Getenv("WAYPOINT_BASH_INIT_SRC") == "" {
					DefaultBashInitSrc = cfg.BashInitSrc
					DefaultBashInitSrcConfig = configSource
				}
				if os.Getenv("WAYPOINT_PRESERVE_SESSION_ON_CLEANUP") == "" {
					PreserveSessionOnCleanup = cfg.PreserveSessionOnCleanup
					PreserveSessionOnCleanupConfig = configSource
				}
			}
		}
	}
}
