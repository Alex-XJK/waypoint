package waypoint

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// checkDir applies one layer to prevState and builds the record at once: the
// call shape the TestCheckDir_* tests use.
func checkDir(rootViewDir, _, targetPath string, prevState *state, blamed *[]BlameCkptRecord, ckptID string) (bool, error) {
	event, err := advance(prevState, rootViewDir, ckptID, targetPath)
	if err != nil || event == nil {
		return false, err
	}
	if blamed != nil {
		record, err := buildBlameRecord(event)
		if err != nil {
			return false, err
		}
		*blamed = append(*blamed, record)
	}
	return true, nil
}

func TestLookupLayer(t *testing.T) {
	const target = "/app/src/main.py"
	cases := []struct {
		name  string
		build func(t *testing.T, layer string)
		want  layerEntry
	}{
		{"empty layer", func(t *testing.T, layer string) {}, layerUntouched},
		{"only directories above the target", func(t *testing.T, layer string) {
			writeTestFile(t, filepath.Join(layer, "app/src/other.py"), "x\n")
		}, layerUntouched},
		{"file at the target", func(t *testing.T, layer string) {
			writeTestFile(t, filepath.Join(layer, "app/src/main.py"), "x\n")
		}, layerPresent},
		{"empty file at the target", func(t *testing.T, layer string) {
			writeTestFile(t, filepath.Join(layer, "app/src/main.py"), "")
		}, layerPresent},
		{"symlink at the target", func(t *testing.T, layer string) {
			writeTestFile(t, filepath.Join(layer, "app/src/other.py"), "x\n")
			if err := os.Symlink("other.py", filepath.Join(layer, "app/src/main.py")); err != nil {
				t.Fatal(err)
			}
		}, layerPresent},
		{"directory at the target", func(t *testing.T, layer string) {
			writeTestFile(t, filepath.Join(layer, "app/src/main.py/inner"), "x\n")
		}, layerHidden},
		{"file above the target", func(t *testing.T, layer string) {
			writeTestFile(t, filepath.Join(layer, "app/src"), "x\n")
		}, layerHidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			layer := t.TempDir()
			tc.build(t, layer)
			got, hostPath, err := lookupLayer(layer, target)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("lookupLayer = %d, want %d", got, tc.want)
			}
			if wantPath := filepath.Join(layer, target); got == layerPresent && hostPath != wantPath {
				t.Fatalf("host path = %q, want %q", hostPath, wantPath)
			}
		})
	}
}

// A symlink above the target hides it. The lookup must not walk through the
// link, which may point outside the layer.
func TestLookupLayerDoesNotFollowSymlinks(t *testing.T) {
	outside := t.TempDir()
	writeTestFile(t, filepath.Join(outside, "src/main.py"), "outside\n")
	layer := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(layer, "app")); err != nil {
		t.Fatal(err)
	}

	got, hostPath, err := lookupLayer(layer, "/app/src/main.py")
	if err != nil {
		t.Fatal(err)
	}
	if got != layerHidden || hostPath != "" {
		t.Fatalf("lookupLayer = %d %q, want hidden and no path", got, hostPath)
	}
}

func TestLookupLayerWhiteoutsAndOpaqueDirectories(t *testing.T) {
	const target = "/app/src/main.py"
	cases := []struct {
		name  string
		build func(t *testing.T, layer string)
		want  layerEntry
	}{
		{"whiteout at the target", func(t *testing.T, layer string) {
			writeTestCharacterDeviceWhiteout(t, filepath.Join(layer, "app/src/main.py"))
		}, layerHidden},
		{"whiteout above the target", func(t *testing.T, layer string) {
			writeTestCharacterDeviceWhiteout(t, filepath.Join(layer, "app/src"))
		}, layerHidden},
		{"xattr whiteout at the target", func(t *testing.T, layer string) {
			writeTestXattrWhiteout(t, filepath.Join(layer, "app/src/main.py"))
		}, layerHidden},
		{"opaque directory without the target", func(t *testing.T, layer string) {
			writeTestOpaqueDirectory(t, filepath.Join(layer, "app"))
		}, layerHidden},
		{"opaque directory two levels above the missing entry", func(t *testing.T, layer string) {
			writeTestOpaqueDirectory(t, filepath.Join(layer, "app"))
			writeTestFile(t, filepath.Join(layer, "app/src/other.py"), "x\n")
		}, layerHidden},
		{"opaque directory with the target", func(t *testing.T, layer string) {
			writeTestOpaqueDirectory(t, filepath.Join(layer, "app"))
			writeTestFile(t, filepath.Join(layer, "app/src/main.py"), "x\n")
		}, layerPresent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			layer := t.TempDir()
			tc.build(t, layer)
			got, _, err := lookupLayer(layer, target)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("lookupLayer = %d, want %d", got, tc.want)
			}
		})
	}
}

func writeTestXattrWhiteout(t *testing.T, path string) {
	t.Helper()
	writeTestFile(t, path, "")
	if err := unix.Setxattr(path, "trusted.overlay.whiteout", []byte("y"), 0); err != nil {
		t.Skipf("cannot set trusted.overlay.whiteout in this environment: %v", err)
	}
}
