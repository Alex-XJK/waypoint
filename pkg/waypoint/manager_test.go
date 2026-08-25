package waypoint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateIDAccepts(t *testing.T) {
	// Every identifier the tool generates or documents must keep working.
	cases := []string{
		MainForkID,
		"A", "B", "C1", "D5", "PS2",
		"fork-0123456789abcdef",  // generateSessionID-derived fork IDs
		"0123456789abcdef",       // session IDs
		"my.checkpoint_v2-final", // all three permitted separators
		strings.Repeat("a", maxIDLength),
	}
	for _, id := range cases {
		t.Run(id, func(t *testing.T) {
			if err := validateID("test", id); err != nil {
				t.Fatalf("validateID(%q) = %v, want nil", id, err)
			}
		})
	}
}

func TestValidateIDRejects(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		// Path traversal: these escape the session tree entirely once joined
		// onto baseDir, so a snapshot could write CRIU images anywhere.
		{"parent dir", ".."},
		{"traversal", "../../../../tmp/escape"},
		{"absolute", "/etc/cron.d/x"},
		{"separator", "a/b"},
		{"dot", "."},
		// OverlayFS mount-option separators: a lowerdir list is colon-joined
		// inside a comma-joined option string, and neither can be escaped.
		{"colon", "x:y"},
		{"comma", "x,y"},
		// Leading punctuation would also let an ID pass for a CLI flag.
		{"leading dash", "-force"},
		{"leading dot", ".hidden"},
		{"leading underscore", "_tmp"},
		{"space", "a b"},
		{"newline", "a\nb"},
		{"nul", "a\x00b"},
		{"too long", strings.Repeat("a", maxIDLength+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateID("test", tc.in); err == nil {
				t.Fatalf("validateID(%q) = nil, want an error", tc.in)
			}
		})
	}
}

// TestValidIDStaysInsideBase is the property the validation exists for: a
// path built from an accepted ID never escapes its parent directory.
func TestValidIDStaysInsideBase(t *testing.T) {
	const base = "/tmp/waypoint-sessions/0123456789abcdef/checkpoints"
	for _, id := range []string{"A", "fork-0123456789abcdef", "my.ckpt_v2-final"} {
		if err := validateID("test", id); err != nil {
			t.Fatalf("validateID(%q) = %v, want nil", id, err)
		}
		joined := filepath.Join(base, id)
		if !strings.HasPrefix(joined, base+"/") {
			t.Fatalf("filepath.Join(%q, %q) = %q, which escapes the base", base, id, joined)
		}
	}
}

func TestValidateCheckpointIDRejectsReserved(t *testing.T) {
	if err := validateCheckpointID("current"); err == nil {
		t.Fatal(`validateCheckpointID("current") = nil, want an error`)
	}
	if err := validateCheckpointID("currently"); err != nil {
		t.Fatalf(`validateCheckpointID("currently") = %v, want nil`, err)
	}
}

func TestMissingForkOperationDoesNotCreateForkDirectory(t *testing.T) {
	m := newManager(t.TempDir())
	const forkID = "missing"

	if _, err := m.ExecuteForkCommand(forkID, "true"); err == nil {
		t.Fatal("ExecuteForkCommand() = nil error, want missing fork error")
	}
	if _, err := os.Stat(m.forkDir(forkID)); !os.IsNotExist(err) {
		t.Fatalf("os.Stat(fork directory) error = %v, want not exist", err)
	}
	if _, err := os.Stat(m.forkLockPath(forkID)); err != nil {
		t.Fatalf("os.Stat(fork lock) error = %v, want lock to exist", err)
	}
}

func TestForkLockSurvivesForkDirectoryRemoval(t *testing.T) {
	m := newManager(t.TempDir())
	const forkID = "removable"

	if err := os.MkdirAll(m.forkDir(forkID), 0o755); err != nil {
		t.Fatalf("MkdirAll(fork directory): %v", err)
	}
	lockPath := m.forkLockPath(forkID)
	if err := m.withForkLock(forkID, func() error {
		return os.RemoveAll(m.forkDir(forkID))
	}); err != nil {
		t.Fatalf("withForkLock(): %v", err)
	}

	if _, err := os.Stat(m.forkDir(forkID)); !os.IsNotExist(err) {
		t.Fatalf("os.Stat(fork directory) error = %v, want not exist", err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("os.Stat(fork lock) error = %v, want lock to survive: %v", err, lockPath)
	}
}
