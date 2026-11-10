package waypoint

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func writeTestCharacterDeviceWhiteout(t *testing.T, path string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}

	// OverlayFS character-device whiteout: device number 0:0.
	dev := int(unix.Mkdev(0, 0))
	if err := unix.Mknod(path, unix.S_IFCHR|0600, dev); err != nil {
		if errors.Is(err, unix.EPERM) ||
			errors.Is(err, unix.EACCES) ||
			errors.Is(err, unix.ENOTSUP) ||
			errors.Is(err, unix.EOPNOTSUPP) {
			t.Skipf("cannot create character-device whiteout in this environment: %v", err)
		}
		t.Fatalf("creating character-device whiteout: %v", err)
	}
}

func writeTestOpaqueDirectory(t *testing.T, path string) {
	t.Helper()

	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatal(err)
	}

	if err := unix.Setxattr(
		path,
		"trusted.overlay.opaque",
		[]byte("y"),
		0,
	); err != nil {
		if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
			t.Skipf("cannot set trusted.overlay.opaque in this environment: %v", err)
		}
		t.Fatalf("setting trusted.overlay.opaque: %v", err)
	}
}

func assertChangeType(t *testing.T, records []BlameCkptRecord, expected ...ChangeType) {
	t.Helper()

	if len(records) != len(expected) {
		t.Fatalf("expected %d blame records, got %d: %+v",
			len(expected), len(records), records)
	}

	for i, change := range expected {
		if records[i].ChangeType != change {
			t.Errorf("record %d: expected change type %v, got %v",
				i, change, records[i].ChangeType)
		}
	}
}

func TestCheckDir_AddFile(t *testing.T) {
	root := t.TempDir()
	target := "/app/file.txt"

	writeTestFile(t, filepath.Join(root, "app/file.txt"), "hello\n")

	prev := &state{
		visible: false,
		ckptID:  "previous",
	}
	var records []BlameCkptRecord

	found, err := checkDir(root, "/", target, prev, &records, "checkpoint-1")
	if err != nil {
		t.Fatal(err)
	}

	if !found {
		t.Fatal("expected target to be found")
	}
	if !prev.visible {
		t.Fatal("expected target to be visible")
	}
	if prev.ckptID != "checkpoint-1" {
		t.Errorf("expected checkpoint-1, got %s", prev.ckptID)
	}

	assertChangeType(t, records, ChangeTypeAdded)
}

func TestCheckDir_ModifyFile(t *testing.T) {
	root := t.TempDir()
	target := "/app/file.txt"

	oldRoot := t.TempDir()
	oldFile := filepath.Join(oldRoot, "file.txt")
	writeTestFile(t, oldFile, "old content\n")

	newFile := filepath.Join(root, "app/file.txt")
	writeTestFile(t, newFile, "new content\n")

	prev := &state{
		visible:      true,
		rootViewPath: oldFile,
		ckptID:       "previous",
	}
	var records []BlameCkptRecord

	found, err := checkDir(root, "/", target, prev, &records, "checkpoint-1")
	if err != nil {
		t.Fatal(err)
	}

	if !found {
		t.Fatal("expected target to be found")
	}

	assertChangeType(t, records, ChangeTypeModified)
}

func TestCheckDir_ReplaceAncestorWithRegularFile(t *testing.T) {
	root := t.TempDir()
	target := "/app/repl/inner.txt"

	// Previous checkpoint: the target exists as a regular file.
	oldRoot := t.TempDir()
	oldFile := filepath.Join(oldRoot, "app", "repl", "inner.txt")
	writeTestFile(t, oldFile, "original content\n")

	prev := state{
		visible:      true,
		rootViewPath: oldFile,
		ckptID:       "checkpoint-1",
	}

	// Current checkpoint: /app/repl is replaced by a regular file.
	writeTestFile(t, filepath.Join(root, "app", "repl"), "replacement\n")

	var records []BlameCkptRecord

	found, err := checkDir(root, "/", target, &prev, &records, "checkpoint-2")
	if err != nil {
		t.Fatal(err)
	}

	if !found {
		t.Fatal("expected ancestor replacement to hide target")
	}
	if prev.visible {
		t.Fatal("expected target to become invisible")
	}
	if prev.ckptID != "checkpoint-2" {
		t.Errorf("expected ckptID checkpoint-2, got %q", prev.ckptID)
	}

	assertChangeType(t, records, ChangeTypeDeleted)
}

func TestCheckDir_ReplaceAncestorWithSymlink(t *testing.T) {
	root := t.TempDir()
	target := "/app/repl/inner.txt"

	// Previous checkpoint: the target exists as a regular file.
	oldRoot := t.TempDir()
	oldFile := filepath.Join(oldRoot, "app", "repl", "inner.txt")
	writeTestFile(t, oldFile, "original content\n")

	prev := &state{
		visible:      true,
		rootViewPath: oldFile,
		ckptID:       "previous",
	}

	// Current checkpoint: /app/repl is replaced by a symlink.
	if err := os.MkdirAll(filepath.Join(root, "app"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/somewhere/else", filepath.Join(root, "app", "repl")); err != nil {
		t.Fatal(err)
	}

	var records []BlameCkptRecord

	found, err := checkDir(root, "/", target, prev, &records, "checkpoint-1")
	if err != nil {
		t.Fatal(err)
	}

	if !found {
		t.Fatal("expected symlink ancestor to hide target")
	}
	if prev.visible {
		t.Fatal("expected target to become invisible")
	}
	if prev.ckptID != "checkpoint-1" {
		t.Errorf("expected ckptID checkpoint-1, got %q", prev.ckptID)
	}

	assertChangeType(t, records, ChangeTypeDeleted)
}

func TestCheckDir_RecreatedDirectoryAddsFile(t *testing.T) {
	target := "/app/repl/inner.txt"

	// Previous checkpoint: the target exists as a regular file.
	oldRoot := t.TempDir()
	oldFile := filepath.Join(oldRoot, "app", "repl", "inner.txt")
	writeTestFile(t, oldFile, "original content\n")

	prev := &state{
		visible:      true,
		rootViewPath: oldFile,
		ckptID:       "previous",
	}
	var records []BlameCkptRecord

	// First checkpoint: replace the directory with a regular file.
	firstLayer := t.TempDir()
	writeTestFile(t, filepath.Join(firstLayer, "app", "repl"), "replacement\n")

	found, err := checkDir(firstLayer, "/", target, prev, &records, "checkpoint-1")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected ancestor replacement to hide target")
	}
	if prev.visible {
		t.Fatal("expected target to become invisible")
	}

	// Second checkpoint: recreate the directory and its original file.
	secondLayer := t.TempDir()
	newFile := filepath.Join(secondLayer, "app", "repl", "inner.txt")
	writeTestFile(t, newFile, "original content\n")

	found, err = checkDir(secondLayer, "/", target, prev, &records, "checkpoint-2")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected recreated target to be found")
	}
	if !prev.visible {
		t.Fatal("expected recreated target to become visible")
	}
	if prev.ckptID != "checkpoint-2" {
		t.Errorf("expected checkpoint-2, got %s", prev.ckptID)
	}

	assertChangeType(t, records,
		ChangeTypeDeleted,
		ChangeTypeAdded,
	)
}

func TestCheckDir_ReplaceAncestorThenModifyRecreatedFile(t *testing.T) {
	target := "/app/repl/inner.txt"

	// Previous checkpoint: the target exists as a regular file.
	oldRoot := t.TempDir()
	oldFile := filepath.Join(oldRoot, "app", "repl", "inner.txt")
	writeTestFile(t, oldFile, "original content\n")

	prev := &state{
		visible:      true,
		rootViewPath: oldFile,
		ckptID:       "previous",
	}
	var records []BlameCkptRecord

	// First checkpoint: replace ancestor with a regular file.
	firstLayer := t.TempDir()
	writeTestFile(t, filepath.Join(firstLayer, "app", "repl"), "replacement\n")

	found, err := checkDir(firstLayer, "/", target, prev, &records, "checkpoint-1")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected ancestor replacement to hide target")
	}
	if prev.visible {
		t.Fatal("expected target to become invisible")
	}

	// Second checkpoint: recreate the target with new content.
	secondLayer := t.TempDir()
	secondFile := filepath.Join(secondLayer, "app", "repl", "inner.txt")
	writeTestFile(t, secondFile, "new content\n")

	found, err = checkDir(secondLayer, "/", target, prev, &records, "checkpoint-2")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected recreated target to be found")
	}
	if !prev.visible {
		t.Fatal("expected recreated target to become visible")
	}

	// Third checkpoint: modify the recreated file.
	thirdLayer := t.TempDir()
	thirdFile := filepath.Join(thirdLayer, "app", "repl", "inner.txt")
	writeTestFile(t, thirdFile, "modified content\n")

	found, err = checkDir(thirdLayer, "/", target, prev, &records, "checkpoint-3")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected modified target to be found")
	}

	if prev.ckptID != "checkpoint-3" {
		t.Errorf("expected checkpoint-3, got %s", prev.ckptID)
	}

	assertChangeType(t, records,
		ChangeTypeDeleted,
		ChangeTypeAdded,
		ChangeTypeModified,
	)
}

func TestCheckDir_DeleteFileWithCharacterDeviceWhiteout(t *testing.T) {
	target := "/app/repl/inner.txt"

	// Previous checkpoint: target exists.
	oldRoot := t.TempDir()
	oldFile := filepath.Join(oldRoot, "app", "repl", "inner.txt")
	writeTestFile(t, oldFile, "original content\n")

	prev := &state{
		visible:      true,
		rootViewPath: oldFile,
		ckptID:       "previous",
	}
	var records []BlameCkptRecord

	// Current checkpoint: target is replaced by a 0:0 character device.
	root := t.TempDir()
	writeTestCharacterDeviceWhiteout(
		t,
		filepath.Join(root, "app", "repl", "inner.txt"),
	)

	found, err := checkDir(root, "/", target, prev, &records, "checkpoint-1")
	if err != nil {
		t.Fatal(err)
	}

	if !found {
		t.Fatal("expected character-device whiteout to hide target")
	}
	if prev.visible {
		t.Fatal("expected target to become invisible")
	}
	if prev.ckptID != "checkpoint-1" {
		t.Errorf("expected checkpoint-1, got %s", prev.ckptID)
	}

	assertChangeType(t, records, ChangeTypeDeleted)
}

func TestCheckDir_DeleteDirectoryWithCharacterDeviceWhiteout(t *testing.T) {
	target := "/app/repl/inner.txt"

	// Previous checkpoint: target exists inside the directory.
	oldRoot := t.TempDir()
	oldFile := filepath.Join(oldRoot, "app", "repl", "inner.txt")
	writeTestFile(t, oldFile, "original content\n")

	prev := &state{
		visible:      true,
		rootViewPath: oldFile,
		ckptID:       "previous",
	}
	var records []BlameCkptRecord

	// Current checkpoint: directory is replaced by a 0:0 character device.
	root := t.TempDir()
	writeTestCharacterDeviceWhiteout(
		t,
		filepath.Join(root, "app", "repl"),
	)

	found, err := checkDir(root, "/", target, prev, &records, "checkpoint-1")
	if err != nil {
		t.Fatal(err)
	}

	if !found {
		t.Fatal("expected character-device whiteout to hide target")
	}
	if prev.visible {
		t.Fatal("expected target to become invisible")
	}
	if prev.ckptID != "checkpoint-1" {
		t.Errorf("expected checkpoint-1, got %s", prev.ckptID)
	}

	assertChangeType(t, records, ChangeTypeDeleted)
}
func TestCheckDir_UnrelatedFileModified_NoBlame(t *testing.T) {
	target := "/app/repl/inner.txt"

	// Previous state: target is visible from an earlier checkpoint.
	oldRoot := t.TempDir()
	oldFile := filepath.Join(oldRoot, "app", "repl", "inner.txt")
	writeTestFile(t, oldFile, "target content\n")

	prev := &state{
		visible:      true,
		rootViewPath: oldFile,
		ckptID:       "checkpoint-1",
	}
	var records []BlameCkptRecord

	// Current layer only modifies an unrelated file. The target does not
	// appear in this layer.
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "app", "other.txt"), "new unrelated content\n")

	found, err := checkDir(root, "/", target, prev, &records, "checkpoint-2")
	if err != nil {
		t.Fatal(err)
	}

	if found {
		t.Fatal("expected no change to be found for the target")
	}

	if !prev.visible {
		t.Fatal("expected target to remain visible")
	}
	if prev.ckptID != "checkpoint-1" {
		t.Errorf("expected checkpoint-1 to remain unchanged, got %s", prev.ckptID)
	}
	if prev.rootViewPath != oldFile {
		t.Errorf("expected rootViewPath to remain %q, got %q", oldFile, prev.rootViewPath)
	}
	if len(records) != 0 {
		t.Fatalf("expected no blame records, got %d: %+v", len(records), records)
	}
}

func TestCheckDir_OpaqueDirectoryDeletesTarget(t *testing.T) {
	target := "/app/repl/inner.txt"

	// Previous checkpoint: the target exists inside the directory.
	oldRoot := t.TempDir()
	oldFile := filepath.Join(oldRoot, "app", "repl", "inner.txt")
	writeTestFile(t, oldFile, "original content\n")

	prev := &state{
		visible:      true,
		rootViewPath: oldFile,
		ckptID:       "previous",
	}
	var records []BlameCkptRecord

	// Current checkpoint: /app/repl is opaque and does not contain the
	// target, so the lower-layer target is hidden.
	root := t.TempDir()
	writeTestOpaqueDirectory(t, filepath.Join(root, "app", "repl"))

	found, err := checkDir(root, "/", target, prev, &records, "checkpoint-1")
	if err != nil {
		t.Fatal(err)
	}

	if !found {
		t.Fatal("expected opaque directory to hide target")
	}
	if prev.visible {
		t.Fatal("expected target to become invisible")
	}
	if prev.ckptID != "checkpoint-1" {
		t.Errorf("expected checkpoint-1, got %q", prev.ckptID)
	}

	assertChangeType(t, records, ChangeTypeDeleted)
}
