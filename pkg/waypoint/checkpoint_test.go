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

func TestRetireNextImagesDir(t *testing.T) {
	m := newManager(t.TempDir())
	// c1/criu <- c1/next/f1-* (as CRIU's restore writes it) <- <ckpt>/criu,
	// the last link only when chained.
	setup := func(t *testing.T, ckpt string, chained bool, nextPages []byte) (f *Fork, criu, rel string) {
		t.Helper()
		if err := os.MkdirAll(m.checkpointCriuDir("c1"), 0o755); err != nil {
			t.Fatal(err)
		}
		next, err := m.newNextImagesDir("c1", "f1")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../../criu", filepath.Join(next, "parent")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(next, "pages-1.img"), nextPages, 0o644); err != nil {
			t.Fatal(err)
		}
		criu = m.checkpointCriuDir(ckpt)
		if err := os.MkdirAll(criu, 0o755); err != nil {
			t.Fatal(err)
		}
		if rel, err = filepath.Rel(criu, next); err != nil {
			t.Fatal(err)
		}
		if chained {
			if err := os.Symlink(rel, filepath.Join(criu, "parent")); err != nil {
				t.Fatal(err)
			}
		}
		return &Fork{ID: "f1", NextImagesDir: next}, criu, rel
	}
	exists := func(path string) bool { _, err := os.Lstat(path); return err == nil }

	// Chained: the link skips the next dir, which goes away.
	f, criu, _ := setup(t, "s1", true, nil)
	next := f.NextImagesDir
	retireNextImagesDir(f, criu)
	if got, _ := os.Readlink(filepath.Join(criu, "parent")); got != "../../c1/criu" {
		t.Errorf("parent link %q, want ../../c1/criu", got)
	}
	if exists(filepath.Join(criu, "parent.tmp")) {
		t.Errorf("parent.tmp left behind")
	}
	if exists(next) || f.NextImagesDir != "" {
		t.Errorf("next dir %s: exists %v, field %q; want removed and cleared", next, exists(next), f.NextImagesDir)
	}

	// A full dump (knob turned off since the restore): nothing to relink.
	f, criu, _ = setup(t, "s2", false, nil)
	next = f.NextImagesDir
	retireNextImagesDir(f, criu)
	if exists(filepath.Join(criu, "parent")) || exists(next) || f.NextImagesDir != "" {
		t.Errorf("full dump: parent %v, next dir %v, field %q; want none", exists(filepath.Join(criu, "parent")), exists(next), f.NextImagesDir)
	}

	// A parent that holds pages is not a next dir: the link and the dir stay,
	// and the fork still drops it.
	f, criu, rel := setup(t, "s3", true, []byte("page"))
	next = f.NextImagesDir
	retireNextImagesDir(f, criu)
	if got, _ := os.Readlink(filepath.Join(criu, "parent")); got != rel {
		t.Errorf("parent link %q, want it unchanged (%q)", got, rel)
	}
	if !exists(next) || f.NextImagesDir != "" {
		t.Errorf("parent with pages: next dir exists %v, field %q; want kept and cleared", exists(next), f.NextImagesDir)
	}
}

func TestDestroyForkRemovesNextImagesDir(t *testing.T) {
	m := newManager(t.TempDir())
	m.sessionID = "test-session"
	next, err := m.newNextImagesDir("c1", "f1")
	if err != nil {
		t.Fatal(err)
	}
	f := &Fork{ID: "f1", SessionID: m.sessionID, RootDir: m.forkDir("f1"), Status: ForkStatusRunning, NextImagesDir: next}
	if err := m.saveFork(f); err != nil {
		t.Fatalf("saveFork: %v", err)
	}
	if err := m.DestroyFork("f1"); err != nil {
		t.Fatalf("DestroyFork: %v", err)
	}
	if _, err := os.Stat(next); !os.IsNotExist(err) {
		t.Errorf("next dir %s after destroy: %v, want removed", next, err)
	}
}
