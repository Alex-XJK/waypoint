package waypoint

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type booleanConfigState struct {
	preserve       bool
	preserveSource string
	tmpfs          bool
	tmpfsSource    string
	phase          bool
	phaseSource    string
	incr           bool
	incrSource     string
}

func currentBooleanConfigState() booleanConfigState {
	return booleanConfigState{
		preserve:       PreserveSessionOnCleanup,
		preserveSource: PreserveSessionOnCleanupConfig,
		tmpfs:          TmpfsImages,
		tmpfsSource:    TmpfsImagesConfig,
		phase:          PhaseStats,
		phaseSource:    PhaseStatsConfig,
		incr:           IncrementalDumps,
		incrSource:     IncrementalDumpsConfig,
	}
}

func setBooleanConfigState(t *testing.T, state booleanConfigState) {
	t.Helper()
	previous := currentBooleanConfigState()
	t.Cleanup(func() {
		applyBooleanConfigState(previous)
	})
	applyBooleanConfigState(state)
}

func applyBooleanConfigState(state booleanConfigState) {
	PreserveSessionOnCleanup = state.preserve
	PreserveSessionOnCleanupConfig = state.preserveSource
	TmpfsImages = state.tmpfs
	TmpfsImagesConfig = state.tmpfsSource
	PhaseStats = state.phase
	PhaseStatsConfig = state.phaseSource
	IncrementalDumps = state.incr
	IncrementalDumpsConfig = state.incrSource
}

func loadBooleanConfigForTest(t *testing.T, contents string, env map[string]string) string {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile(config): %v", err)
	}

	for _, key := range []string{
		"WAYPOINT_SESSION_INFO_DIR",
		"WAYPOINT_SESSIONS_DIR",
		"WAYPOINT_BASH_INIT_SRC",
		"WAYPOINT_PRESERVE_SESSION_ON_CLEANUP",
		"WAYPOINT_TMPFS_IMAGES",
		"WAYPOINT_TMPFS_DIR",
		"WAYPOINT_PHASE_STATS",
		"WAYPOINT_INCREMENTAL_DUMPS",
		"WAYPOINT_LOG_LEVEL",
		"WAYPOINT_NETWORK",
	} {
		t.Setenv(key, "")
	}
	for key, value := range env {
		t.Setenv(key, value)
	}
	t.Setenv("WAYPOINT_CONFIG", cfgPath)

	loadConfig()
	return "config:" + cfgPath
}

func TestLoadConfigAppliesExplicitFalseBooleans(t *testing.T) {
	setBooleanConfigState(t, booleanConfigState{
		preserve:       true,
		preserveSource: "previous",
		tmpfs:          true,
		tmpfsSource:    "previous",
		phase:          true,
		phaseSource:    "previous",
		incr:           true,
		incrSource:     "previous",
	})
	source := loadBooleanConfigForTest(t, `{
		"preserve_session_on_cleanup": false,
		"tmpfs_images": false,
		"phase_stats": false,
		"incremental_dumps": false
	}`, nil)

	want := booleanConfigState{
		preserveSource: source,
		tmpfsSource:    source,
		phaseSource:    source,
		incrSource:     source,
	}
	if got := currentBooleanConfigState(); got != want {
		t.Fatalf("boolean config state = %+v, want %+v", got, want)
	}
}

func TestLoadConfigLeavesOmittedBooleansAndSourcesUntouched(t *testing.T) {
	want := booleanConfigState{
		preserveSource: "default",
		tmpfsSource:    "default",
		phaseSource:    "default",
		incrSource:     "default",
	}
	setBooleanConfigState(t, want)
	loadBooleanConfigForTest(t, `{}`, nil)

	if got := currentBooleanConfigState(); got != want {
		t.Fatalf("boolean config state = %+v, want %+v", got, want)
	}
}

func TestLoadConfigBooleanEnvironmentOverridesFile(t *testing.T) {
	setBooleanConfigState(t, booleanConfigState{})
	loadBooleanConfigForTest(t, `{
		"preserve_session_on_cleanup": true,
		"tmpfs_images": true,
		"phase_stats": true,
		"incremental_dumps": true
	}`, map[string]string{
		"WAYPOINT_PRESERVE_SESSION_ON_CLEANUP": "false",
		"WAYPOINT_TMPFS_IMAGES":                "false",
		"WAYPOINT_PHASE_STATS":                 "false",
		"WAYPOINT_INCREMENTAL_DUMPS":           "false",
	})

	want := booleanConfigState{
		preserveSource: "env:WAYPOINT_PRESERVE_SESSION_ON_CLEANUP",
		tmpfsSource:    "env:WAYPOINT_TMPFS_IMAGES",
		phaseSource:    "env:WAYPOINT_PHASE_STATS",
		incrSource:     "env:WAYPOINT_INCREMENTAL_DUMPS",
	}
	if got := currentBooleanConfigState(); got != want {
		t.Fatalf("boolean config state = %+v, want %+v", got, want)
	}
}

func TestLoadConfigInvalidBooleanEnvironmentFallsBackToFile(t *testing.T) {
	setBooleanConfigState(t, booleanConfigState{
		preserveSource: "default",
		tmpfsSource:    "default",
		phaseSource:    "default",
		incrSource:     "default",
	})
	source := loadBooleanConfigForTest(t, `{
		"preserve_session_on_cleanup": true,
		"tmpfs_images": true,
		"phase_stats": true,
		"incremental_dumps": true
	}`, map[string]string{
		"WAYPOINT_PRESERVE_SESSION_ON_CLEANUP": "not-a-bool",
		"WAYPOINT_TMPFS_IMAGES":                "not-a-bool",
		"WAYPOINT_PHASE_STATS":                 "not-a-bool",
		"WAYPOINT_INCREMENTAL_DUMPS":           "not-a-bool",
	})

	want := booleanConfigState{
		preserve:       true,
		preserveSource: source,
		tmpfs:          true,
		tmpfsSource:    source,
		phase:          true,
		phaseSource:    source,
		incr:           true,
		incrSource:     source,
	}
	if got := currentBooleanConfigState(); got != want {
		t.Fatalf("boolean config state = %+v, want %+v", got, want)
	}
}

func TestLoadConfigAppliesExplicitTrueBooleans(t *testing.T) {
	// The complement of the explicit-false case: a file must be able to turn
	// a default-off flag on, and say so in the reported source.
	setBooleanConfigState(t, booleanConfigState{
		preserveSource: "default",
		tmpfsSource:    "default",
		phaseSource:    "default",
		incrSource:     "default",
	})
	source := loadBooleanConfigForTest(t, `{
		"preserve_session_on_cleanup": true,
		"tmpfs_images": true,
		"phase_stats": true,
		"incremental_dumps": true
	}`, nil)

	want := booleanConfigState{
		preserve:       true,
		preserveSource: source,
		tmpfs:          true,
		tmpfsSource:    source,
		phase:          true,
		phaseSource:    source,
		incr:           true,
		incrSource:     source,
	}
	if got := currentBooleanConfigState(); got != want {
		t.Fatalf("boolean config state = %+v, want %+v", got, want)
	}
}

// TestLoadConfigIsIdempotent guards against the resolution drifting when it
// runs more than once: every command re-resolves, and LoadConfigInfo resolves
// again on top of whatever a Manager already did.
func TestLoadConfigIsIdempotent(t *testing.T) {
	setBooleanConfigState(t, booleanConfigState{})
	contents := `{"preserve_session_on_cleanup": true, "tmpfs_images": false}`
	loadBooleanConfigForTest(t, contents, nil)
	first := currentBooleanConfigState()
	loadConfig()
	if second := currentBooleanConfigState(); second != first {
		t.Fatalf("second loadConfig() = %+v, want %+v", second, first)
	}
}

func TestLoadConfigNetworkMode(t *testing.T) {
	cases := []struct {
		name       string
		file       string
		env        string
		wantMode   string
		wantSource string // "file" stands for the config file's source label
	}{
		{"default", `{}`, "", NetworkShared, "default"},
		{"file", `{"network": "private"}`, "", NetworkPrivate, "file"},
		{"environment overrides file", `{"network": "shared"}`, "private", NetworkPrivate, "env:WAYPOINT_NETWORK"},
		{"invalid environment falls back to file", `{"network": "private"}`, "bogus", NetworkPrivate, "file"},
		{"invalid file value is ignored", `{"network": "bogus"}`, "", NetworkShared, "default"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			previousMode, previousSource := NetworkMode, NetworkModeConfig
			t.Cleanup(func() { NetworkMode, NetworkModeConfig = previousMode, previousSource })
			NetworkMode, NetworkModeConfig = NetworkShared, "default"

			source := loadBooleanConfigForTest(t, tc.file, map[string]string{"WAYPOINT_NETWORK": tc.env})
			wantSource := tc.wantSource
			if wantSource == "file" {
				wantSource = source
			}
			if NetworkMode != tc.wantMode || NetworkModeConfig != wantSource {
				t.Fatalf("network mode = %q from %q, want %q from %q", NetworkMode, NetworkModeConfig, tc.wantMode, wantSource)
			}
		})
	}
}

func TestLoadConfigLogLevel(t *testing.T) {
	cases := []struct {
		name       string
		file       string
		env        string
		wantLevel  int
		wantSource string // "file" stands for the config file's source label
	}{
		{"default", `{}`, "", 2, "default"},
		{"file", `{"waypoint_log_level": 4}`, "", 4, "file"},
		{"file zero is applied", `{"waypoint_log_level": 0}`, "", 0, "file"},
		{"environment overrides file", `{"waypoint_log_level": 4}`, "1", 1, "env:WAYPOINT_LOG_LEVEL"},
		{"invalid environment falls back to file", `{"waypoint_log_level": 3}`, "debug", 3, "file"},
		{"out of range environment falls back to file", `{"waypoint_log_level": 3}`, "5", 3, "file"},
		{"invalid file value is ignored", `{"waypoint_log_level": 9}`, "", 2, "default"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			previousLevel, previousSource := LogLevel, LogLevelConfig
			t.Cleanup(func() { LogLevel, LogLevelConfig = previousLevel, previousSource })
			LogLevel, LogLevelConfig = 2, "default"

			source := loadBooleanConfigForTest(t, tc.file, map[string]string{"WAYPOINT_LOG_LEVEL": tc.env})
			wantSource := tc.wantSource
			if wantSource == "file" {
				wantSource = source
			}
			if LogLevel != tc.wantLevel || LogLevelConfig != wantSource {
				t.Fatalf("criu log level = %d from %q, want %d from %q", LogLevel, LogLevelConfig, tc.wantLevel, wantSource)
			}
			if got, want := criuLogLevelArg(), "-v"+strconv.Itoa(tc.wantLevel); got != want {
				t.Fatalf("criuLogLevelArg() = %q, want %q", got, want)
			}
		})
	}
}

func TestCriuLogTail(t *testing.T) {
	previous := LogLevel
	t.Cleanup(func() { LogLevel = previous })
	path := filepath.Join(t.TempDir(), "restore.log")
	if err := os.WriteFile(path, []byte("Error (criu/cr-restore.c:1): boom\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	LogLevel = 2
	tail := criuLogTail(path)
	if !strings.Contains(tail, path) || !strings.Contains(tail, "boom") || !strings.Contains(tail, "WAYPOINT_LOG_LEVEL=4") {
		t.Fatalf("criuLogTail at level 2 = %q, want the path, the error and the level hint", tail)
	}
	LogLevel = 4
	if tail := criuLogTail(path); strings.Contains(tail, "WAYPOINT_LOG_LEVEL") {
		t.Fatalf("criuLogTail at level 4 = %q, want no level hint", tail)
	}
}

func TestIncrementalDumpsOffWithTmpfsImages(t *testing.T) {
	cases := []struct {
		incr, tmpfs bool
		want        bool
		wantSource  string
	}{
		{false, false, false, "file"},
		{true, false, true, "file"},
		{true, true, false, "file (off: tmpfs_images)"},
		{false, true, false, "file"},
	}
	for _, tc := range cases {
		setBooleanConfigState(t, booleanConfigState{incr: tc.incr, incrSource: "file", tmpfs: tc.tmpfs})
		if got := incrementalDumps(); got != tc.want {
			t.Errorf("incrementalDumps() with incremental=%v tmpfs=%v = %v, want %v", tc.incr, tc.tmpfs, got, tc.want)
		}
		info := incrementalDumpsInfo()
		if info.Value != tc.want || info.Source != tc.wantSource {
			t.Errorf("incrementalDumpsInfo() with incremental=%v tmpfs=%v = %v from %q, want %v from %q",
				tc.incr, tc.tmpfs, info.Value, info.Source, tc.want, tc.wantSource)
		}
	}
}
