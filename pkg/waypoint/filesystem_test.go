package waypoint

import (
	"os"
	"os/exec"
	"testing"
)

func TestUnescapeMountField(t *testing.T) {
	cases := map[string]string{
		`/plain/path`:       "/plain/path",
		`/mnt/my\040dir`:    "/mnt/my dir",
		`/a\011b\012c\134d`: "/a\tb\nc\\d",
	}
	for in, want := range cases {
		if got := unescapeMountField(in); got != want {
			t.Errorf("unescapeMountField(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMountPointCount(t *testing.T) {
	if n, err := mountPointCount("/"); err != nil || n < 1 {
		t.Errorf("mountPointCount(/) = %d, %v; want >=1, nil", n, err)
	}
	if n, err := mountPointCount("/waypoint/not/a/mount"); err != nil || n != 0 {
		t.Errorf("mountPointCount(bogus) = %d, %v; want 0, nil", n, err)
	}
	if !isMountPoint("/") {
		t.Error("isMountPoint(/) = false, want true")
	}
	if isMountPoint("/waypoint/not/a/mount") {
		t.Error("isMountPoint(bogus) = true, want false")
	}
}

// TestUnmountAllPeelsStackedMounts covers the case that leaves sessions behind:
// when an unmount fails, the next mount stacks another layer on the same path,
// and a teardown that unmounts once leaves the rest of the stack in place.
func TestUnmountAllPeelsStackedMounts(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to mount")
	}

	dir := t.TempDir()
	const layers = 3
	for i := 0; i < layers; i++ {
		if err := exec.Command("mount", "-t", "tmpfs", "tmpfs", dir).Run(); err != nil {
			t.Fatalf("mount layer %d: %v", i, err)
		}
	}
	m := &Manager{workOverlay: dir}
	t.Cleanup(func() { _ = m.unmountAll(dir) })

	if n, _ := mountPointCount(dir); n != layers {
		t.Fatalf("stacked mount count = %d, want %d", n, layers)
	}

	// A single unmount only peels the topmost layer.
	if err := exec.Command("umount", dir).Run(); err != nil {
		t.Fatalf("single umount: %v", err)
	}
	if n, _ := mountPointCount(dir); n != layers-1 {
		t.Fatalf("after one umount, count = %d, want %d", n, layers-1)
	}

	if err := m.unmountAll(dir); err != nil {
		t.Fatalf("unmountAll: %v", err)
	}
	if n, _ := mountPointCount(dir); n != 0 {
		t.Errorf("after unmountAll, count = %d, want 0", n)
	}
}

func TestUnmountAllOnUnmountedPathIsNoOp(t *testing.T) {
	m := &Manager{}
	if err := m.unmountAll(t.TempDir()); err != nil {
		t.Errorf("unmountAll on plain directory: %v", err)
	}
	if err := m.unmountAll(""); err != nil {
		t.Errorf("unmountAll on empty path: %v", err)
	}
}
