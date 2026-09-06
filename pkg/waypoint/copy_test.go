package waypoint

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCopyToAndFromForkRegularFile(t *testing.T) {
	m, _ := newCopyTestManager(t)
	hostSource := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(hostSource, []byte("uploaded"), 0o751); err != nil {
		t.Fatal(err)
	}
	guestDestination := filepath.Join(t.TempDir(), "nested", "guest-file")

	if err := m.CopyToFork("f1", hostSource, guestDestination); err != nil {
		t.Fatalf("CopyToFork(): %v", err)
	}
	assertFileContents(t, guestDestination, "uploaded")
	assertPermissions(t, guestDestination, 0o751)

	if err := os.WriteFile(guestDestination, []byte("downloaded"), 0o751); err != nil {
		t.Fatal(err)
	}
	hostDestination := filepath.Join(t.TempDir(), "nested", "host-file")
	if err := os.MkdirAll(filepath.Dir(hostDestination), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hostDestination, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := m.CopyFromFork("f1", guestDestination, hostDestination); err != nil {
		t.Fatalf("CopyFromFork(): %v", err)
	}
	assertFileContents(t, hostDestination, "downloaded")
	assertPermissions(t, hostDestination, 0o751)
	assertNoCopyTemps(t, filepath.Dir(hostDestination))
}

func TestCopyDirectoryMergesExistingDestination(t *testing.T) {
	m, _ := newCopyTestManager(t)
	source := t.TempDir()
	writeCopyTestFile(t, filepath.Join(source, "replace"), "new", 0o640)
	writeCopyTestFile(t, filepath.Join(source, "new-dir", "new"), "new-child", 0o750)
	if err := os.Chmod(filepath.Join(source, "new-dir"), 0o710); err != nil {
		t.Fatal(err)
	}

	destination := filepath.Join(t.TempDir(), "destination")
	writeCopyTestFile(t, filepath.Join(destination, "replace"), "old", 0o600)
	writeCopyTestFile(t, filepath.Join(destination, "keep"), "keep", 0o600)
	writeCopyTestFile(t, filepath.Join(destination, "new-dir", "keep-child"), "keep-child", 0o600)
	if err := os.Chmod(destination, 0o701); err != nil {
		t.Fatal(err)
	}

	if err := m.CopyToFork("f1", source, destination); err != nil {
		t.Fatalf("CopyToFork(directory): %v", err)
	}
	assertFileContents(t, filepath.Join(destination, "replace"), "new")
	assertFileContents(t, filepath.Join(destination, "keep"), "keep")
	assertFileContents(t, filepath.Join(destination, "new-dir", "new"), "new-child")
	assertFileContents(t, filepath.Join(destination, "new-dir", "keep-child"), "keep-child")
	assertPermissions(t, destination, 0o701)
	assertNoCopyTemps(t, destination)
}

func TestCopyDirectoryContentsUsesPinnedSourceDirectory(t *testing.T) {
	parent := t.TempDir()
	sourcePath := filepath.Join(parent, "source")
	writeCopyTestFile(t, filepath.Join(sourcePath, "value"), "original", 0o600)

	sourceDir, info, err := openCopySource(hostCopySource{}, sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer sourceDir.Close()
	if !info.IsDir() {
		t.Fatal("opened source is not a directory")
	}

	if err := os.Rename(sourcePath, filepath.Join(parent, "moved")); err != nil {
		t.Fatal(err)
	}
	writeCopyTestFile(t, filepath.Join(sourcePath, "value"), "replacement", 0o600)

	destinationPath := t.TempDir()
	destination, err := os.OpenRoot(destinationPath)
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	if err := copyDirectoryContents(sourcePath, sourceDir, destination); err != nil {
		t.Fatalf("copyDirectoryContents(): %v", err)
	}
	assertFileContents(t, filepath.Join(destinationPath, "value"), "original")
}

func TestCopyFromForkDirectoryMergesExistingDestination(t *testing.T) {
	m, _ := newCopyTestManager(t)
	guestSource := t.TempDir()
	writeCopyTestFile(t, filepath.Join(guestSource, "replace"), "guest", 0o644)
	writeCopyTestFile(t, filepath.Join(guestSource, "empty", ".keep"), "", 0o600)
	if err := os.Remove(filepath.Join(guestSource, "empty", ".keep")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(guestSource, "empty"), 0o711); err != nil {
		t.Fatal(err)
	}

	hostDestination := filepath.Join(t.TempDir(), "destination")
	writeCopyTestFile(t, filepath.Join(hostDestination, "replace"), "host", 0o600)
	writeCopyTestFile(t, filepath.Join(hostDestination, "keep"), "keep", 0o600)

	if err := m.CopyFromFork("f1", guestSource, hostDestination); err != nil {
		t.Fatalf("CopyFromFork(directory): %v", err)
	}
	assertFileContents(t, filepath.Join(hostDestination, "replace"), "guest")
	assertFileContents(t, filepath.Join(hostDestination, "keep"), "keep")
	assertPermissions(t, filepath.Join(hostDestination, "empty"), 0o711)
}

func TestCopyRequiresRunningVerifiedFork(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Fork)
		want   string
	}{
		{
			name: "not running",
			mutate: func(f *Fork) {
				f.Status = ForkStatusSnapshot
			},
			want: "is not running",
		},
		{
			name: "no pid",
			mutate: func(f *Fork) {
				f.PID = 0
			},
			want: "no verifiable process identity",
		},
		{
			name: "no start time",
			mutate: func(f *Fork) {
				f.StartTime = 0
			},
			want: "no verifiable process identity",
		},
		{
			name: "changed identity",
			mutate: func(f *Fork) {
				f.StartTime++
			},
			want: "process identity",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m, fork := newCopyTestManager(t)
			test.mutate(fork)
			if err := m.saveFork(fork); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(t.TempDir(), "source")
			if err := os.WriteFile(source, []byte("data"), 0o600); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "destination")

			err := m.CopyToFork("f1", source, destination)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("CopyToFork() error = %v, want containing %q", err, test.want)
			}
			if _, err := os.Stat(destination); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("destination exists after rejected copy: %v", err)
			}
		})
	}
}

func TestCopyValidatesForkIDBeforeCreatingLock(t *testing.T) {
	m, _ := newCopyTestManager(t)
	err := m.CopyToFork("../bad", "unused", "/unused")
	if err == nil || !strings.Contains(err.Error(), "invalid fork ID") {
		t.Fatalf("CopyToFork() error = %v, want invalid fork ID", err)
	}
	if _, err := os.Stat(filepath.Join(m.baseDir, "bad.lock")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("invalid fork created a lock path: %v", err)
	}
}

func TestCopyWaitsForForkLock(t *testing.T) {
	m, _ := newCopyTestManager(t)
	source := filepath.Join(t.TempDir(), "source")
	destination := filepath.Join(t.TempDir(), "destination")
	if err := os.WriteFile(source, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	acquired := make(chan struct{})
	release := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- m.withForkLock("f1", func() error {
			close(acquired)
			<-release
			return nil
		})
	}()
	<-acquired

	copyDone := make(chan error, 1)
	go func() {
		copyDone <- m.CopyToFork("f1", source, destination)
	}()
	select {
	case err := <-copyDone:
		t.Fatalf("CopyToFork() completed while fork lock was held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-lockDone; err != nil {
		t.Fatalf("lock holder: %v", err)
	}
	if err := <-copyDone; err != nil {
		t.Fatalf("CopyToFork() after releasing lock: %v", err)
	}
}

func TestCopyRootRejectsTraversal(t *testing.T) {
	rootPath := t.TempDir()
	outsidePath := t.TempDir()
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	outsideDestination := filepath.Join(outsidePath, "escaped")
	if err := os.WriteFile(outsideDestination, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if err := copyIntoRoot(hostCopySource{}, source, root, "../"+filepath.Base(outsidePath)+"/escaped"); err == nil {
		t.Fatal("copyIntoRoot() with .. destination succeeded")
	}
	assertFileContents(t, outsideDestination, "sentinel")

	relativeLink := filepath.Join(rootPath, "relative-link")
	if err := os.Symlink(filepath.Join("..", filepath.Base(outsidePath)), relativeLink); err != nil {
		t.Fatal(err)
	}
	if err := copyIntoRoot(hostCopySource{}, source, root, "relative-link/escaped"); err == nil {
		t.Fatal("copyIntoRoot() through relative escape symlink succeeded")
	}
	assertFileContents(t, outsideDestination, "sentinel")
}

func TestCopyRootRejectsAbsoluteSymlinkComponents(t *testing.T) {
	rootPath := t.TempDir()
	outsidePath := t.TempDir()
	outsideFile := filepath.Join(outsidePath, "secret")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsidePath, filepath.Join(rootPath, "absolute-link")); err != nil {
		t.Fatal(err)
	}
	hostSource := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(hostSource, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if err := copyIntoRoot(hostCopySource{}, hostSource, root, "absolute-link/secret"); err == nil {
		t.Fatal("upload through absolute symlink succeeded")
	}
	assertFileContents(t, outsideFile, "secret")

	hostDestinationDir := t.TempDir()
	hostDestinationRoot, err := os.OpenRoot(hostDestinationDir)
	if err != nil {
		t.Fatal(err)
	}
	defer hostDestinationRoot.Close()
	if err := copyIntoRoot(rootCopySource{root: root}, "absolute-link/secret", hostDestinationRoot, "copied"); err == nil {
		t.Fatal("download through absolute symlink succeeded")
	}
	if _, err := os.Stat(filepath.Join(hostDestinationDir, "copied")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("download destination exists after rejected copy: %v", err)
	}
}

func TestCleanRootPathRemovesTrailingSlash(t *testing.T) {
	tests := map[string]string{
		"/":           ".",
		"dir/":        "dir",
		"/dir///":     "dir",
		"/dir/./file": "dir/file",
	}
	for input, want := range tests {
		got, err := cleanRootPath(input)
		if err != nil {
			t.Fatalf("cleanRootPath(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("cleanRootPath(%q) = %q, want %q", input, got, want)
		}
	}
	for _, input := range []string{"", "..", "../outside", "/../outside"} {
		if _, err := cleanRootPath(input); err == nil {
			t.Fatalf("cleanRootPath(%q) succeeded, want error", input)
		}
	}
}

func TestCopyRejectsSourceSymlinksAndSpecialFiles(t *testing.T) {
	destinationPath := t.TempDir()
	destination, err := os.OpenRoot(destinationPath)
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()

	sourceDir := t.TempDir()
	writeCopyTestFile(t, filepath.Join(sourceDir, "a-regular"), "data", 0o600)
	if err := os.Symlink("a-regular", filepath.Join(sourceDir, "z-link")); err != nil {
		t.Fatal(err)
	}
	err = copyIntoRoot(hostCopySource{}, sourceDir, destination, "staged-directory")
	if err == nil || !strings.Contains(err.Error(), "symbolic links are not supported") {
		t.Fatalf("copyIntoRoot(symlink tree) error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(destinationPath, "staged-directory")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("failed staged directory is visible: %v", err)
	}
	assertNoCopyTemps(t, destinationPath)

	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	err = copyIntoRoot(hostCopySource{}, fifo, destination, "fifo")
	if err == nil || !strings.Contains(err.Error(), "special file type") {
		t.Fatalf("copyIntoRoot(fifo) error = %v", err)
	}
}

func TestCopyToForkRejectsTrailingSlashSymlinkSource(t *testing.T) {
	m, _ := newCopyTestManager(t)
	sourceDir := t.TempDir()
	target := filepath.Join(sourceDir, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(sourceDir, "link")
	if err := os.Symlink("target", link); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "destination")

	for _, source := range []string{link + string(filepath.Separator), filepath.Join(link, ".")} {
		err := m.CopyToFork("f1", source, destination)
		if err == nil || !strings.Contains(err.Error(), "symbolic links are not supported") {
			t.Fatalf("CopyToFork(%q) error = %v, want symlink rejection", source, err)
		}
		if _, err := os.Stat(destination); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("destination exists after rejected symlink source: %v", err)
		}
	}
}

func TestCopyRejectsDestinationTypeConflicts(t *testing.T) {
	destinationPath := t.TempDir()
	destination, err := os.OpenRoot(destinationPath)
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()

	fileSource := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(fileSource, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkTarget := filepath.Join(destinationPath, "target")
	if err := os.WriteFile(linkTarget, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(destinationPath, "link")); err != nil {
		t.Fatal(err)
	}
	if err := copyIntoRoot(hostCopySource{}, fileSource, destination, "link"); err == nil {
		t.Fatal("regular file replaced a destination symlink")
	}
	assertFileContents(t, linkTarget, "sentinel")

	directorySource := t.TempDir()
	if err := copyIntoRoot(hostCopySource{}, directorySource, destination, "target"); err == nil {
		t.Fatal("directory replaced a destination regular file")
	}
	assertFileContents(t, linkTarget, "sentinel")
	assertNoCopyTemps(t, destinationPath)
}

func newCopyTestManager(t *testing.T) (*Manager, *Fork) {
	t.Helper()
	m := newManager(t.TempDir())
	m.sessionID = "test-session"
	startTime, err := procStartTime(os.Getpid())
	if err != nil {
		t.Fatalf("procStartTime(self): %v", err)
	}
	f := &Fork{
		ID:        "f1",
		SessionID: m.sessionID,
		RootDir:   m.forkDir("f1"),
		PID:       os.Getpid(),
		StartTime: startTime,
		Status:    ForkStatusRunning,
	}
	if err := m.saveFork(f); err != nil {
		t.Fatalf("saveFork(): %v", err)
	}
	return m, f
}

func writeCopyTestFile(t *testing.T, name, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}

func assertFileContents(t *testing.T, name, want string) {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", name, err)
	}
	if string(data) != want {
		t.Fatalf("ReadFile(%s) = %q, want %q", name, data, want)
	}
}

func assertPermissions(t *testing.T, name string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(name)
	if err != nil {
		t.Fatalf("Stat(%s): %v", name, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("permissions for %s = %o, want %o", name, got, want)
	}
}

func assertNoCopyTemps(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".waypoint-copy-") {
			t.Fatalf("temporary copy entry was not cleaned up: %s", filepath.Join(dir, entry.Name()))
		}
	}
}
