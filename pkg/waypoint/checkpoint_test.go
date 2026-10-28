package waypoint

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadCheckpointMetadataLoadsConfig(t *testing.T) {
	// Prevent the caller's environment from changing unrelated configuration
	// globals. Each case supplies an explicit config file, avoiding host config.
	for _, key := range []string{
		"WAYPOINT_SESSION_INFO_DIR",
		"WAYPOINT_SESSIONS_DIR",
		"WAYPOINT_BASH_INIT_SRC",
		"WAYPOINT_PRESERVE_SESSION_ON_CLEANUP",
		"WAYPOINT_TMPFS_IMAGES",
		"WAYPOINT_TMPFS_DIR",
		"WAYPOINT_PHASE_STATS",
		"WAYPOINT_INCREMENTAL_DUMPS",
		"WAYPOINT_NETWORK",
	} {
		t.Setenv(key, "")
	}

	for _, source := range []string{"environment", "config_file"} {
		t.Run(source, func(t *testing.T) {
			previousDir, previousSource := SessionInfoDir, SessionInfoDirConfig
			t.Cleanup(func() {
				SessionInfoDir, SessionInfoDirConfig = previousDir, previousSource
			})
			// Start with a registry that contains no session, as a fresh CLI
			// process does before resolving the user's custom configuration.
			SessionInfoDir, SessionInfoDirConfig = t.TempDir(), "default"
			registryDir := t.TempDir()
			sessionDir := t.TempDir()
			metadataDir := filepath.Join(sessionDir, "metadata")
			if err := os.Mkdir(metadataDir, 0o755); err != nil {
				t.Fatalf("Mkdir(metadata): %v", err)
			}

			writeJSON := func(path string, value any) {
				t.Helper()
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatalf("Marshal(%s): %v", path, err)
				}
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatalf("WriteFile(%s): %v", path, err)
				}
			}
			want := Metadata{
				ID:                "candidate",
				SessionID:         "session",
				ParentID:          "prepared",
				LayerIDs:          []string{"prepared", "candidate"},
				CreatedFromForkID: "branch",
				Status:            CheckpointStatusReady,
			}
			// Write fixtures directly: constructing a Manager or loading session
			// info here would resolve config first and hide the regression.
			writeJSON(filepath.Join(registryDir, "session.json"), SessionInfo{
				SessionID: "session",
				BaseDir:   sessionDir,
			})
			writeJSON(filepath.Join(metadataDir, "candidate.json"), want)

			cfg := config{SessionInfoDir: registryDir}
			if source == "environment" {
				// The environment must also win over a different file registry.
				cfg.SessionInfoDir = t.TempDir()
				t.Setenv("WAYPOINT_SESSION_INFO_DIR", registryDir)
			}
			cfgPath := filepath.Join(t.TempDir(), "config.json")
			writeJSON(cfgPath, cfg)
			t.Setenv("WAYPOINT_CONFIG", cfgPath)

			got, err := LoadCheckpointMetadata("session", "candidate")
			if err != nil {
				t.Fatalf("LoadCheckpointMetadata(): %v", err)
			}
			if !reflect.DeepEqual(got, &want) {
				t.Fatalf("LoadCheckpointMetadata() = %+v, want %+v", got, &want)
			}
		})
	}
}

func TestNextImagesDirs(t *testing.T) {
	m := newManager(t.TempDir())
	plain, tmpfs := m.checkpointCriuDir("plain"), m.checkpointCriuDir("tmpfs")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatalf("MkdirAll(plain): %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(tmpfs), 0o755); err != nil {
		t.Fatalf("MkdirAll(tmpfs parent): %v", err)
	}
	if err := os.Symlink(t.TempDir(), tmpfs); err != nil {
		t.Fatalf("Symlink(tmpfs): %v", err)
	}
	for id, want := range map[string]bool{"plain": true, "tmpfs": false, "missing": false} {
		if got := m.chainsFrom(id); got != want {
			t.Errorf("chainsFrom(%s) = %v, want %v", id, got, want)
		}
	}

	// A fork recreated under the same ID gets a new dir next to the old one.
	first, err := m.newNextImagesDir("plain", "f1")
	if err != nil {
		t.Fatalf("newNextImagesDir: %v", err)
	}
	second, err := m.newNextImagesDir("plain", "f1")
	if err != nil {
		t.Fatalf("newNextImagesDir: %v", err)
	}
	wantParent := filepath.Join(m.checkpointDir("plain"), "next")
	for _, dir := range []string{first, second} {
		if !filepath.IsAbs(dir) || filepath.Dir(dir) != wantParent || !strings.HasPrefix(filepath.Base(dir), "f1-") {
			t.Errorf("next images dir %s, want an absolute %s/f1-*", dir, wantParent)
		}
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Errorf("next images dir %s: %d entries, err %v; want empty", dir, len(entries), err)
		}
	}
	if first == second {
		t.Errorf("newNextImagesDir returned %s twice", first)
	}
}
