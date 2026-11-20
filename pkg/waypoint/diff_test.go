package waypoint

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/checkpoint-restore/go-criu/v8/crit"
	criu_core "github.com/checkpoint-restore/go-criu/v8/crit/images/criu-core"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/pstree"
	"golang.org/x/sys/unix"
)

// Test collectCandidatesDir
func skipIfUnsupported(t *testing.T, err error) {
	t.Helper()

	if errors.Is(err, unix.EPERM) ||
		errors.Is(err, unix.EACCES) ||
		errors.Is(err, unix.EOPNOTSUPP) ||
		errors.Is(err, unix.ENOTSUP) {
		t.Skipf("requires privileged operation or support: %v", err)
	}

	t.Fatalf("unexpected error: %v", err)
}

func TestCollectCandidateDir(t *testing.T) {
	tests := []struct {
		name                string
		setup               func(t *testing.T, root string)
		expectedCandidates  map[string]struct{}
		expectedDirectories map[string]struct{}
	}{
		{
			name: "regular file",
			setup: func(t *testing.T, root string) {
				dir := filepath.Join(root, "app")
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "file"), []byte("content"), 0644); err != nil {
					t.Fatal(err)
				}
			},
			expectedCandidates: map[string]struct{}{
				"/app/file": {},
			},
			expectedDirectories: map[string]struct{}{
				"/app": {},
			},
		},
		{
			name: "empty directories",
			setup: func(t *testing.T, root string) {
				if err := os.MkdirAll(filepath.Join(root, "app", "data", "nested"), 0755); err != nil {
					t.Fatal(err)
				}
			},
			expectedCandidates: map[string]struct{}{},
			expectedDirectories: map[string]struct{}{
				"/app":             {},
				"/app/data":        {},
				"/app/data/nested": {},
			},
		},
		{
			name: "character device whiteout",
			setup: func(t *testing.T, root string) {
				dir := filepath.Join(root, "app")
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
				createTestWhiteoutDevice(t, filepath.Join(dir, "whiteout"))
			},
			expectedCandidates: map[string]struct{}{
				"/app/whiteout": {},
			},
			expectedDirectories: map[string]struct{}{
				"/app": {},
			},
		},
		{
			name: "xattr whiteout",
			setup: func(t *testing.T, root string) {
				dir := filepath.Join(root, "app")
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
				createTestWhiteoutXattr(t, filepath.Join(dir, "whiteout"))
			},
			expectedCandidates: map[string]struct{}{
				"/app/whiteout": {},
			},
			expectedDirectories: map[string]struct{}{
				"/app": {},
			},
		},
		{
			name: "empty opaque directory",
			setup: func(t *testing.T, root string) {
				dir := filepath.Join(root, "app", "data")
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
				makeTestPathOpaque(t, dir)
			},
			expectedCandidates: map[string]struct{}{},
			expectedDirectories: map[string]struct{}{
				"/app":      {},
				"/app/data": {},
			},
		},
		{
			name: "opaque directory containing file",
			setup: func(t *testing.T, root string) {
				dir := filepath.Join(root, "app", "data")
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
				makeTestPathOpaque(t, dir)

				if err := os.WriteFile(filepath.Join(dir, "file"), []byte("content"), 0644); err != nil {
					t.Fatal(err)
				}
			},
			expectedCandidates: map[string]struct{}{
				"/app/data/file": {},
			},
			expectedDirectories: map[string]struct{}{
				"/app":      {},
				"/app/data": {},
			},
		},
		{
			name: "nested empty opaque directory",
			setup: func(t *testing.T, root string) {
				dir := filepath.Join(root, "app", "data", "nested")
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
				makeTestPathOpaque(t, dir)
			},
			expectedCandidates: map[string]struct{}{},
			expectedDirectories: map[string]struct{}{
				"/app":             {},
				"/app/data":        {},
				"/app/data/nested": {},
			},
		},
		{
			name: "mixed contents",
			setup: func(t *testing.T, root string) {
				appDir := filepath.Join(root, "app")
				configDir := filepath.Join(appDir, "config")
				dataDir := filepath.Join(appDir, "data")

				if err := os.MkdirAll(configDir, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(dataDir, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte("{}"), 0644); err != nil {
					t.Fatal(err)
				}
				createTestWhiteoutDevice(t, filepath.Join(appDir, "whiteout"))
				makeTestPathOpaque(t, dataDir)
			},
			expectedCandidates: map[string]struct{}{
				"/app/config/settings.json": {},
				"/app/whiteout":             {},
			},
			expectedDirectories: map[string]struct{}{
				"/app":        {},
				"/app/config": {},
				"/app/data":   {},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			tt.setup(t, root)

			view := &fsView{
				candidates:         make(map[string]struct{}),
				dirsWithCandidates: make(map[string]struct{}),
			}

			if err := view.collectCandidateDir(root, "/"); err != nil {
				t.Fatalf("collectCandidateDir: %v", err)
			}

			if !reflect.DeepEqual(view.candidates, tt.expectedCandidates) {
				t.Errorf("candidates:\n got: %#v\nwant: %#v",
					view.candidates, tt.expectedCandidates)
			}

			if !reflect.DeepEqual(view.dirsWithCandidates, tt.expectedDirectories) {
				t.Errorf("dirsWithCandidates:\n got: %#v\nwant: %#v",
					view.dirsWithCandidates, tt.expectedDirectories)
			}
		})
	}
}

// helpers for TestCollectCandidateDir
func createTestWhiteoutDevice(t *testing.T, path string) {
	t.Helper()
	err := unix.Mknod(
		path,
		unix.S_IFCHR|0600,
		int(unix.Mkdev(0, 0)),
	)
	if err != nil {
		skipIfUnsupported(t, err)
	}
}
func createTestWhiteoutXattr(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatalf("create file: %v", err)
	}
	if err := unix.Setxattr(
		path,
		"trusted.overlay.whiteout",
		[]byte("y"),
		0,
	); err != nil {
		skipIfUnsupported(t, err)
	}
}
func makeTestPathOpaque(t *testing.T, path string) {
	t.Helper()
	if err := unix.Setxattr(
		path,
		"trusted.overlay.opaque",
		[]byte("y"),
		0,
	); err != nil {
		skipIfUnsupported(t, err)
	}
}

func TestWalkFS(t *testing.T) {
	tests := []struct {
		name string

		leftFiles  map[string]string
		rightFiles map[string]string

		leftDirs  []string
		rightDirs []string

		rightModes map[string]os.FileMode // applied after the right tree is created

		candidates         map[string]struct{}
		dirsWithCandidates map[string]struct{}

		want []expectedDiff
	}{
		{
			name:       "directory_mode_change_is_metadata",
			leftDirs:   []string{"/app/data"},
			rightDirs:  []string{"/app/data"},
			rightModes: map[string]os.FileMode{"/app/data": 0700},
			dirsWithCandidates: map[string]struct{}{
				"/app":      {},
				"/app/data": {},
			},
			want: []expectedDiff{
				{path: "/app/data", changeType: ChangeTypeMetadata, fileType: FileTypeDir, checkTypes: true},
			},
		},
		{
			name:      "deleted_directory_tree",
			leftDirs:  []string{"/app/old/sub", "/app/old/empty"},
			leftFiles: map[string]string{"/app/old/sub/f.txt": "x"},
			rightDirs: []string{"/app"},
			dirsWithCandidates: map[string]struct{}{
				"/app": {},
			},
			want: []expectedDiff{
				{path: "/app/old", changeType: ChangeTypeDeleted, fileType: FileTypeDir, checkTypes: true},
				{path: "/app/old/sub", changeType: ChangeTypeDeleted, fileType: FileTypeDir, checkTypes: true},
				{path: "/app/old/empty", changeType: ChangeTypeDeleted, fileType: FileTypeDir, checkTypes: true},
				{path: "/app/old/sub/f.txt", changeType: ChangeTypeDeleted, fileType: FileTypeRegular, checkTypes: true},
			},
		},
		{
			name:       "directory_with_subdirectory_to_file",
			leftDirs:   []string{"/app/data/sub"},
			leftFiles:  map[string]string{"/app/data/sub/f.txt": "x"},
			rightFiles: map[string]string{"/app/data": "now a file"},
			candidates: map[string]struct{}{"/app/data": {}},
			dirsWithCandidates: map[string]struct{}{
				"/app": {},
			},
			want: []expectedDiff{
				{
					path:       "/app/data",
					changeType: ChangeTypeModified,
					fileType:   FileTypeModified,
					fromType:   FileTypeDir,
					toType:     FileTypeRegular,
					checkTypes: true,
				},
				{path: "/app/data/sub", changeType: ChangeTypeDeleted, fileType: FileTypeDir, checkTypes: true},
				{path: "/app/data/sub/f.txt", changeType: ChangeTypeDeleted, fileType: FileTypeRegular, checkTypes: true},
			},
		},
		{
			name:       "mode_only_change_is_metadata",
			leftFiles:  map[string]string{"/app/run.sh": "echo hi"},
			rightFiles: map[string]string{"/app/run.sh": "echo hi"},
			rightModes: map[string]os.FileMode{"/app/run.sh": 0755},
			candidates: map[string]struct{}{"/app/run.sh": {}},
			dirsWithCandidates: map[string]struct{}{
				"/app": {},
			},
			want: []expectedDiff{
				{path: "/app/run.sh", changeType: ChangeTypeMetadata, fileType: FileTypeRegular, checkTypes: true},
			},
		},
		{
			name:       "mode_and_content_change_is_modified",
			leftFiles:  map[string]string{"/app/run.sh": "echo hi"},
			rightFiles: map[string]string{"/app/run.sh": "echo yo"},
			rightModes: map[string]os.FileMode{"/app/run.sh": 0755},
			candidates: map[string]struct{}{"/app/run.sh": {}},
			dirsWithCandidates: map[string]struct{}{
				"/app": {},
			},
			want: []expectedDiff{
				{path: "/app/run.sh", changeType: ChangeTypeModified, fileType: FileTypeRegular, checkTypes: true},
			},
		},
		{
			name: "identical_candidate_files",
			leftFiles: map[string]string{
				"/app/file.txt": "same content",
			},
			rightFiles: map[string]string{
				"/app/file.txt": "same content",
			},
			candidates: map[string]struct{}{
				"/app/file.txt": {},
			},
			dirsWithCandidates: map[string]struct{}{
				"/app": {},
			},
			want: nil,
		},
		{
			name:     "added_file",
			leftDirs: []string{"/app"},
			rightFiles: map[string]string{
				"/app/new.txt": "new content",
			},
			candidates: map[string]struct{}{
				"/app/new.txt": {},
			},
			dirsWithCandidates: map[string]struct{}{
				"/app": {},
			},
			want: []expectedDiff{
				{path: "/app/new.txt", changeType: ChangeTypeAdded},
			},
		},
		{
			name: "deleted_file",
			leftFiles: map[string]string{
				"/app/old.txt": "old content",
			},
			rightDirs: []string{"/app"},
			candidates: map[string]struct{}{
				"/app/old.txt": {},
			},
			dirsWithCandidates: map[string]struct{}{
				"/app": {},
			},
			want: []expectedDiff{
				{path: "/app/old.txt", changeType: ChangeTypeDeleted},
			},
		},
		{
			name: "non_candidate_is_ignored",
			leftFiles: map[string]string{
				"/app/file.txt": "some content",
			},
			rightFiles: map[string]string{
				"/app/file.txt": "other content",
			},
			candidates: map[string]struct{}{},
			dirsWithCandidates: map[string]struct{}{
				"/app": {},
			},
			want: nil,
		},
		{
			name: "modified_nested_candidate",
			leftFiles: map[string]string{
				"/app/sub/file.txt": "left content",
			},
			rightFiles: map[string]string{
				"/app/sub/file.txt": "right content",
			},
			candidates: map[string]struct{}{
				"/app/sub/file.txt": {},
			},
			dirsWithCandidates: map[string]struct{}{
				"/app":     {},
				"/app/sub": {},
			},
			want: []expectedDiff{
				{path: "/app/sub/file.txt", changeType: ChangeTypeModified},
			},
		},
		{
			name: "file_to_directory",
			leftFiles: map[string]string{
				"/app/data": "old file",
			},
			rightDirs: []string{
				"/app/data",
			},
			rightFiles: map[string]string{
				"/app/data/new.txt": "new file",
			},
			candidates: map[string]struct{}{
				"/app/data":         {},
				"/app/data/new.txt": {},
			},
			dirsWithCandidates: map[string]struct{}{
				"/app":      {},
				"/app/data": {},
			},
			want: []expectedDiff{
				{
					path:       "/app/data",
					changeType: ChangeTypeModified,
					fileType:   FileTypeModified,
					fromType:   getFileType(os.FileMode(0)),
					toType:     FileTypeDir,
					checkTypes: true,
				},
				{path: "/app/data/new.txt", changeType: ChangeTypeAdded},
			},
		},
		{
			name: "directory_to_file",
			leftDirs: []string{
				"/app/data",
			},
			leftFiles: map[string]string{
				"/app/data/old.txt": "old file",
			},
			rightFiles: map[string]string{
				"/app/data": "replacement file",
			},
			candidates: map[string]struct{}{
				"/app/data":         {},
				"/app/data/old.txt": {},
			},
			dirsWithCandidates: map[string]struct{}{
				"/app":      {},
				"/app/data": {},
			},
			want: []expectedDiff{
				{
					path:       "/app/data",
					changeType: ChangeTypeModified,
					fileType:   FileTypeModified,
					fromType:   FileTypeDir,
					toType:     getFileType(os.FileMode(0)),
					checkTypes: true,
				},
				{path: "/app/data/old.txt", changeType: ChangeTypeDeleted},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			leftPath := t.TempDir()
			rightPath := t.TempDir()

			createTestTree(t, leftPath, tt.leftDirs, tt.leftFiles)
			createTestTree(t, rightPath, tt.rightDirs, tt.rightFiles)
			for path, mode := range tt.rightModes {
				if err := os.Chmod(e2ePath(rightPath, path), mode); err != nil {
					t.Fatalf("chmod %s: %v", path, err)
				}
			}

			leftRoot, err := os.OpenRoot(leftPath)
			if err != nil {
				t.Fatalf("open left root: %v", err)
			}
			defer leftRoot.Close()

			rightRoot, err := os.OpenRoot(rightPath)
			if err != nil {
				t.Fatalf("open right root: %v", err)
			}
			defer rightRoot.Close()

			view := &fsView{
				leftRoot:           leftRoot,
				rightRoot:          rightRoot,
				candidates:         tt.candidates,
				dirsWithCandidates: tt.dirsWithCandidates,
			}

			var detected []DiffFileRecord

			if err := walkFS(view, "/", &detected); err != nil {
				t.Fatalf("walkFS: %v", err)
			}

			assertDiffRecords(t, detected, tt.want)
		})
	}
}

func TestListDirReturnsEntryTypes(t *testing.T) {
	path := t.TempDir()
	createTestTree(t, path, []string{"/app/sub"}, map[string]string{"/app/file": "x"})
	if err := os.Symlink("sub", filepath.Join(path, "app/link")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(path, "app/pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if err := os.Chmod(filepath.Join(path, "app"), 0o750); err != nil {
		t.Fatal(err)
	}
	got, dirMode, err := listDir(root, "app")
	if err != nil {
		t.Fatalf("listDir: %v", err)
	}
	if dirMode != fs.ModeDir|0o750 {
		t.Errorf("listDir directory mode = %v, want %v", dirMode, fs.ModeDir|0o750)
	}
	want := map[string]fs.FileMode{"sub": fs.ModeDir, "file": 0, "link": fs.ModeSymlink, "pipe": fs.ModeNamedPipe}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("listDir = %v, want %v", got, want)
	}
	// Listing twice reads the directory from the start both times.
	if again, _, err := listDir(root, "app"); err != nil || !reflect.DeepEqual(again, want) {
		t.Errorf("second listDir = %v, %v", again, err)
	}
	if _, _, err := listDir(root, "missing"); err == nil {
		t.Error("listDir of a missing directory succeeded")
	}
}

// One-sided entries, directories included, take their file type from the directory listing.
func TestWalkFSReportsTypesOfOneSidedEntries(t *testing.T) {
	leftPath, rightPath := t.TempDir(), t.TempDir()
	createTestTree(t, leftPath, []string{"/app"}, nil)
	createTestTree(t, rightPath, []string{"/app/new/empty"}, map[string]string{
		"/app/new/file.txt":      "x",
		"/app/new/deep/file.txt": "y",
	})
	if err := os.Symlink("file.txt", filepath.Join(rightPath, "app/new/link")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(rightPath, "app/new/pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := map[string]FileType{
		"/app/new":               FileTypeDir,
		"/app/new/empty":         FileTypeDir,
		"/app/new/deep":          FileTypeDir,
		"/app/new/file.txt":      FileTypeRegular,
		"/app/new/deep/file.txt": FileTypeRegular,
		"/app/new/link":          FileTypeSymlink,
		"/app/new/pipe":          FileTypeFIFO,
	}
	for _, tc := range []struct {
		left, right string
		change      ChangeType
	}{{leftPath, rightPath, ChangeTypeAdded}, {rightPath, leftPath, ChangeTypeDeleted}} {
		leftRoot, err := os.OpenRoot(tc.left)
		if err != nil {
			t.Fatal(err)
		}
		defer leftRoot.Close()
		rightRoot, err := os.OpenRoot(tc.right)
		if err != nil {
			t.Fatal(err)
		}
		defer rightRoot.Close()
		view := &fsView{leftRoot: leftRoot, rightRoot: rightRoot, dirsWithCandidates: map[string]struct{}{"/app": {}}}
		var detected []DiffFileRecord
		if err := walkFS(view, "/", &detected); err != nil {
			t.Fatalf("walkFS: %v", err)
		}
		got := map[string]FileType{}
		for _, r := range detected {
			if r.ChangeType != tc.change {
				t.Errorf("%s: change type %s, want %s", r.Path, r.ChangeType, tc.change)
			}
			got[r.Path] = r.FileType
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s records:\n got: %v\nwant: %v", tc.change, got, want)
		}
	}
}

func setCompareWorkers(t *testing.T, n int) {
	t.Helper()
	prev := compareWorkers
	t.Cleanup(func() { compareWorkers = prev })
	compareWorkers = n
}

// Every candidate is compared exactly once, whatever the number of workers.
func TestWalkFSComparesEveryCandidate(t *testing.T) {
	leftFiles, rightFiles := map[string]string{}, map[string]string{}
	candidates, dirs := map[string]struct{}{}, map[string]struct{}{}
	var want []expectedDiff
	for i := 0; i < 600; i++ {
		dir := fmt.Sprintf("/app/d%02d", i%20)
		path := fmt.Sprintf("%s/f%03d.txt", dir, i)
		dirs["/app"], dirs[dir] = struct{}{}, struct{}{}
		candidates[path] = struct{}{}
		leftFiles[path] = fmt.Sprintf("content %d", i)
		rightFiles[path] = leftFiles[path]
		if i%3 == 0 {
			rightFiles[path] = fmt.Sprintf("changed %d", i)
			want = append(want, expectedDiff{path: path, changeType: ChangeTypeModified})
		}
	}
	leftPath, rightPath := t.TempDir(), t.TempDir()
	createTestTree(t, leftPath, nil, leftFiles)
	createTestTree(t, rightPath, nil, rightFiles)
	leftRoot, err := os.OpenRoot(leftPath)
	if err != nil {
		t.Fatal(err)
	}
	defer leftRoot.Close()
	rightRoot, err := os.OpenRoot(rightPath)
	if err != nil {
		t.Fatal(err)
	}
	defer rightRoot.Close()

	for _, workers := range []int{1, 3, 16} {
		t.Run(fmt.Sprintf("workers_%d", workers), func(t *testing.T) {
			setCompareWorkers(t, workers)
			view := &fsView{leftRoot: leftRoot, rightRoot: rightRoot, candidates: candidates, dirsWithCandidates: dirs}
			var detected []DiffFileRecord
			if err := walkFS(view, "/", &detected); err != nil {
				t.Fatalf("walkFS: %v", err)
			}
			assertDiffRecords(t, detected, want)
		})
	}
}

func TestComparePoolReturnsFirstError(t *testing.T) {
	leftPath, rightPath := t.TempDir(), t.TempDir()
	files := map[string]string{}
	for i := 0; i < 100; i++ {
		files[fmt.Sprintf("/f%03d", i)] = "same"
	}
	createTestTree(t, leftPath, nil, files)
	createTestTree(t, rightPath, nil, files)
	leftRoot, err := os.OpenRoot(leftPath)
	if err != nil {
		t.Fatal(err)
	}
	defer leftRoot.Close()
	rightRoot, err := os.OpenRoot(rightPath)
	if err != nil {
		t.Fatal(err)
	}
	defer rightRoot.Close()

	pool := newComparePool(&fsView{leftRoot: leftRoot, rightRoot: rightRoot})
	if err := pool.submit("/missing"); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	// The queue keeps accepting paths until a worker has failed; none may block.
	var submitErr error
	for i := 0; i < 100 && submitErr == nil; i++ {
		submitErr = pool.submit(fmt.Sprintf("/f%03d", i))
		time.Sleep(time.Millisecond)
	}
	if submitErr == nil || !strings.Contains(submitErr.Error(), "missing") {
		t.Errorf("submit after a failed comparison = %v, want the error on /missing", submitErr)
	}
	records, err := pool.wait()
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("wait = %v, want the error on /missing", err)
	}
	if len(records) != 0 {
		t.Errorf("records = %v, want none", records)
	}
}

// One pair of buffers serves files of any size, in any order.
func TestAreRegFilesDiffReusesBuffers(t *testing.T) {
	big := strings.Repeat("0123456789abcdef", 6000) // 96 KB, three chunks
	lateChange := big[:len(big)-1] + "X"
	cases := []struct {
		name        string
		left, right string
		want        bool
	}{
		{"big_same", big, big, false},
		{"small_differs", "abc", "abd", true},
		{"small_same", "abc", "abc", false},
		{"big_differs_in_last_chunk", big, lateChange, true},
		{"empty", "", "", false},
		{"size_differs", "abc", "abcd", true},
		{"one_chunk_same", big[:32*1024], big[:32*1024], false},
	}
	leftPath, rightPath := t.TempDir(), t.TempDir()
	for _, tc := range cases {
		createTestTree(t, leftPath, nil, map[string]string{tc.name: tc.left})
		createTestTree(t, rightPath, nil, map[string]string{tc.name: tc.right})
	}
	leftRoot, err := os.OpenRoot(leftPath)
	if err != nil {
		t.Fatal(err)
	}
	defer leftRoot.Close()
	rightRoot, err := os.OpenRoot(rightPath)
	if err != nil {
		t.Fatal(err)
	}
	defer rightRoot.Close()

	bufs := newCompareBufs()
	for round := 0; round < 2; round++ {
		for _, tc := range cases {
			got, err := areRegFilesDiff(leftRoot, rightRoot, tc.name, bufs)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if got != tc.want {
				t.Errorf("round %d, %s: differ = %v, want %v", round, tc.name, got, tc.want)
			}
		}
	}
}

type expectedDiff struct {
	path       string
	changeType ChangeType

	fileType FileType
	fromType FileType
	toType   FileType

	checkTypes bool
}

// helpers for TestWalkFS
func createTestTree(
	t *testing.T,
	root string,
	dirs []string,
	files map[string]string,
) {
	t.Helper()

	for _, dir := range dirs {
		path := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(dir, "/")))
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatalf("create directory %s: %v", path, err)
		}
	}

	for file, content := range files {
		path := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(file, "/")))

		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatalf("create parent directory for %s: %v", path, err)
		}

		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("write file %s: %v", path, err)
		}
	}
}

func assertDiffRecords(
	t *testing.T,
	got []DiffFileRecord,
	want []expectedDiff,
) {
	t.Helper()

	gotByPath := make(map[string]DiffFileRecord, len(got))
	for _, record := range got {
		if _, exists := gotByPath[record.Path]; exists {
			t.Errorf("duplicate diff record for path %s", record.Path)
		}
		gotByPath[record.Path] = record
	}

	if len(gotByPath) != len(want) {
		t.Errorf("got %d diff records, want %d", len(gotByPath), len(want))
	}

	for _, expected := range want {
		record, exists := gotByPath[expected.path]
		if !exists {
			t.Errorf("missing diff record for %s", expected.path)
			continue
		}

		if record.ChangeType != expected.changeType {
			t.Errorf(
				"%s: ChangeType = %v, want %v",
				expected.path,
				record.ChangeType,
				expected.changeType,
			)
		}

		if expected.checkTypes {
			if record.FileType != expected.fileType {
				t.Errorf(
					"%s: FileType = %v, want %v",
					expected.path,
					record.FileType,
					expected.fileType,
				)
			}

			if record.FromFileType != expected.fromType {
				t.Errorf(
					"%s: FromFileType = %v, want %v",
					expected.path,
					record.FromFileType,
					expected.fromType,
				)
			}

			if record.ToFileType != expected.toType {
				t.Errorf(
					"%s: ToFileType = %v, want %v",
					expected.path,
					record.ToFileType,
					expected.toType,
				)
			}
		}

		delete(gotByPath, expected.path)
	}

	for path := range gotByPath {
		t.Errorf("unexpected diff record for %s", path)
	}
}

func TestDiffFSOverlayE2E(t *testing.T) {
	tests := []e2eDiffTest{
		{
			name: "file_added",
			baseActions: []func(*testing.T, string){
				func(t *testing.T, root string) {
					e2eMkdir(t, root, "/app")
					e2eWriteFile(t, root, "/app/existing.txt", "original")
				},
			},
			layers: []e2eLayer{
				{name: "left"},
				{
					name: "right",
					actions: []func(*testing.T, string){
						func(t *testing.T, root string) {
							e2eWriteFile(t, root, "/app/new.txt", "new")
						},
					},
				},
			},
			leftLayers:  []int{0},
			rightLayers: []int{1},
			want: []expectedDiff{
				{path: "/app/new.txt", changeType: ChangeTypeAdded},
			},
		},
		{
			name: "file_deleted",
			baseActions: []func(*testing.T, string){
				func(t *testing.T, root string) {
					e2eMkdir(t, root, "/app")
					e2eWriteFile(t, root, "/app/deleted.txt", "original")
				},
			},
			layers: []e2eLayer{
				{name: "left"},
				{
					name: "right",
					actions: []func(*testing.T, string){
						func(t *testing.T, root string) {
							e2eRemove(t, root, "/app/deleted.txt")
						},
					},
				},
			},
			leftLayers:  []int{0},
			rightLayers: []int{1},
			want: []expectedDiff{
				{path: "/app/deleted.txt", changeType: ChangeTypeDeleted},
			},
		},
		{
			name: "file_modified",
			baseActions: []func(*testing.T, string){
				func(t *testing.T, root string) {
					e2eMkdir(t, root, "/app")
					e2eWriteFile(t, root, "/app/file.txt", "original")
				},
			},
			layers: []e2eLayer{
				{name: "left"},
				{
					name: "right",
					actions: []func(*testing.T, string){
						func(t *testing.T, root string) {
							e2eWriteFile(t, root, "/app/file.txt", "modified")
						},
					},
				},
			},
			leftLayers:  []int{0},
			rightLayers: []int{1},
			want: []expectedDiff{
				{path: "/app/file.txt", changeType: ChangeTypeModified},
			},
		},
		{
			name: "empty_directory_added",
			baseActions: []func(*testing.T, string){
				func(t *testing.T, root string) {
					e2eMkdir(t, root, "/app")
				},
			},
			layers: []e2eLayer{
				{name: "left"},
				{
					name: "right",
					actions: []func(*testing.T, string){
						func(t *testing.T, root string) {
							e2eMkdir(t, root, "/app/empty")
						},
					},
				},
			},
			leftLayers:  []int{0},
			rightLayers: []int{1},
			want: []expectedDiff{
				{path: "/app/empty", changeType: ChangeTypeAdded, fileType: FileTypeDir, checkTypes: true},
			},
		},
		{
			name: "directory_with_file_added",
			baseActions: []func(*testing.T, string){
				func(t *testing.T, root string) {
					e2eMkdir(t, root, "/app")
				},
			},
			layers: []e2eLayer{
				{name: "left"},
				{
					name: "right",
					actions: []func(*testing.T, string){
						func(t *testing.T, root string) {
							e2eWriteFile(t, root, "/app/new/file.txt", "new")
						},
					},
				},
			},
			leftLayers:  []int{0},
			rightLayers: []int{1},
			want: []expectedDiff{
				{path: "/app/new", changeType: ChangeTypeAdded, fileType: FileTypeDir, checkTypes: true},
				{path: "/app/new/file.txt", changeType: ChangeTypeAdded},
			},
		},
		{
			name: "file_to_directory",
			baseActions: []func(*testing.T, string){
				func(t *testing.T, root string) {
					e2eMkdir(t, root, "/app")
					e2eWriteFile(t, root, "/app/data", "old file")
				},
			},
			layers: []e2eLayer{
				{name: "left"},
				{
					name: "right",
					actions: []func(*testing.T, string){
						func(t *testing.T, root string) {
							e2eRemove(t, root, "/app/data")
							e2eMkdir(t, root, "/app/data")
							e2eWriteFile(t, root, "/app/data/new.txt", "new")
						},
					},
				},
			},
			leftLayers:  []int{0},
			rightLayers: []int{1},
			want: []expectedDiff{
				{
					path:       "/app/data",
					changeType: ChangeTypeModified,
					fileType:   FileTypeModified,
					fromType:   getFileType(os.FileMode(0)),
					toType:     FileTypeDir,
					checkTypes: true,
				},
				{path: "/app/data/new.txt", changeType: ChangeTypeAdded},
			},
		},
		{
			name: "directory_to_file",
			baseActions: []func(*testing.T, string){
				func(t *testing.T, root string) {
					e2eMkdir(t, root, "/app/data")
					e2eWriteFile(t, root, "/app/data/old.txt", "old")
				},
			},
			layers: []e2eLayer{
				{name: "left"},
				{
					name: "right",
					actions: []func(*testing.T, string){
						func(t *testing.T, root string) {
							e2eRemove(t, root, "/app/data")
							e2eWriteFile(t, root, "/app/data", "replacement")
						},
					},
				},
			},
			leftLayers:  []int{0},
			rightLayers: []int{1},
			want: []expectedDiff{
				{
					path:       "/app/data",
					changeType: ChangeTypeModified,
					fileType:   FileTypeModified,
					fromType:   FileTypeDir,
					toType:     getFileType(os.FileMode(0)),
					checkTypes: true,
				},
				{path: "/app/data/old.txt", changeType: ChangeTypeDeleted},
			},
		},
		{
			name: "replace_directory",
			baseActions: []func(*testing.T, string){
				func(t *testing.T, root string) {
					e2eMkdir(t, root, "/app/data")
					e2eWriteFile(t, root, "/app/data/old.txt", "old")
				},
			},
			layers: []e2eLayer{
				{name: "left"},
				{
					name: "right",
					actions: []func(*testing.T, string){
						func(t *testing.T, root string) {
							e2eRemove(t, root, "/app/data")
							e2eMkdir(t, root, "/app/data")
						},
					},
				},
			},
			leftLayers:  []int{0},
			rightLayers: []int{1},
			want: []expectedDiff{
				{path: "/app/data/old.txt", changeType: ChangeTypeDeleted},
			},
		},
		{
			name: "shared_layer_is_not_a_candidate",
			baseActions: []func(*testing.T, string){
				func(t *testing.T, root string) {
					e2eMkdir(t, root, "/app")
				},
			},
			layers: []e2eLayer{
				{
					name: "shared",
					actions: []func(*testing.T, string){
						func(t *testing.T, root string) {
							e2eWriteFile(t, root, "/app/shared.txt", "same")
						},
					},
				},
				{
					name:        "left",
					lowerLayers: []int{0},
				},
				{
					name:        "right",
					lowerLayers: []int{0},
				},
			},
			leftLayers:  []int{0, 1},
			rightLayers: []int{0, 2},
			want:        nil,
		},
		{
			name: "replace_directory_keeps_subdir",
			baseActions: []func(*testing.T, string){
				func(t *testing.T, root string) {
					e2eWriteFile(t, root, "/app/data/sub/old.txt", "old")
					e2eWriteFile(t, root, "/app/data/sub/deeper/older.txt", "older")
				},
			},
			layers: []e2eLayer{
				{name: "left"},
				{
					name: "right",
					actions: []func(*testing.T, string){
						func(t *testing.T, root string) {
							e2eRemove(t, root, "/app/data")
							e2eMkdir(t, root, "/app/data/sub/deeper")
						},
					},
				},
			},
			leftLayers:  []int{0},
			rightLayers: []int{1},
			want: []expectedDiff{
				{path: "/app/data/sub/old.txt", changeType: ChangeTypeDeleted},
				{path: "/app/data/sub/deeper/older.txt", changeType: ChangeTypeDeleted},
			},
		},
		{
			name: "remove_then_recreate_in_later_layer",
			baseActions: []func(*testing.T, string){
				func(t *testing.T, root string) {
					e2eWriteFile(t, root, "/app/data/sub/old.txt", "old")
				},
			},
			layers: []e2eLayer{
				{name: "left"},
				{
					name: "remove",
					actions: []func(*testing.T, string){
						func(t *testing.T, root string) {
							e2eRemove(t, root, "/app/data")
						},
					},
				},
				{
					name:        "recreate",
					lowerLayers: []int{1},
					actions: []func(*testing.T, string){
						func(t *testing.T, root string) {
							e2eMkdir(t, root, "/app/data/sub")
						},
					},
				},
			},
			leftLayers:  []int{0},
			rightLayers: []int{1, 2},
			want: []expectedDiff{
				{path: "/app/data/sub/old.txt", changeType: ChangeTypeDeleted},
			},
		},
		{
			name: "recreated_subdir_reversed",
			baseActions: []func(*testing.T, string){
				func(t *testing.T, root string) {
					e2eWriteFile(t, root, "/app/data/sub/old.txt", "old")
				},
			},
			layers: []e2eLayer{
				{
					name: "left",
					actions: []func(*testing.T, string){
						func(t *testing.T, root string) {
							e2eRemove(t, root, "/app/data")
							e2eMkdir(t, root, "/app/data/sub")
						},
					},
				},
				{name: "right"},
			},
			leftLayers:  []int{0},
			rightLayers: []int{1},
			want: []expectedDiff{
				{path: "/app/data/sub/old.txt", changeType: ChangeTypeAdded},
			},
		},
		{
			name: "chmod_directory",
			baseActions: []func(*testing.T, string){
				func(t *testing.T, root string) {
					e2eWriteFile(t, root, "/app/data/keep.txt", "keep")
				},
			},
			layers: []e2eLayer{
				{name: "left"},
				{
					name: "right",
					actions: []func(*testing.T, string){
						func(t *testing.T, root string) {
							e2eChmod(t, root, "/app/data", 0700)
						},
					},
				},
			},
			leftLayers:  []int{0},
			rightLayers: []int{1},
			want: []expectedDiff{
				{path: "/app/data", changeType: ChangeTypeMetadata, fileType: FileTypeDir, checkTypes: true},
			},
		},
		{
			name: "directory_tree_removed",
			baseActions: []func(*testing.T, string){
				func(t *testing.T, root string) {
					e2eWriteFile(t, root, "/app/data/sub/f.txt", "x")
					e2eWriteFile(t, root, "/app/keep.txt", "keep")
				},
			},
			layers: []e2eLayer{
				{name: "left"},
				{
					name: "right",
					actions: []func(*testing.T, string){
						func(t *testing.T, root string) {
							e2eRemove(t, root, "/app/data")
						},
					},
				},
			},
			leftLayers:  []int{0},
			rightLayers: []int{1},
			want: []expectedDiff{
				{path: "/app/data", changeType: ChangeTypeDeleted, fileType: FileTypeDir, checkTypes: true},
				{path: "/app/data/sub", changeType: ChangeTypeDeleted, fileType: FileTypeDir, checkTypes: true},
				{path: "/app/data/sub/f.txt", changeType: ChangeTypeDeleted, fileType: FileTypeRegular, checkTypes: true},
			},
		},
		{
			name: "chmod_only",
			baseActions: []func(*testing.T, string){
				func(t *testing.T, root string) {
					e2eWriteFile(t, root, "/app/run.sh", "echo hi")
				},
			},
			layers: []e2eLayer{
				{name: "left"},
				{
					name: "right",
					actions: []func(*testing.T, string){
						func(t *testing.T, root string) {
							e2eChmod(t, root, "/app/run.sh", 0755)
						},
					},
				},
			},
			leftLayers:  []int{0},
			rightLayers: []int{1},
			want: []expectedDiff{
				{path: "/app/run.sh", changeType: ChangeTypeMetadata, fileType: FileTypeRegular, checkTypes: true},
			},
		},
		{
			name: "chmod_and_write",
			baseActions: []func(*testing.T, string){
				func(t *testing.T, root string) {
					e2eWriteFile(t, root, "/app/run.sh", "echo hi")
				},
			},
			layers: []e2eLayer{
				{name: "left"},
				{
					name: "right",
					actions: []func(*testing.T, string){
						func(t *testing.T, root string) {
							e2eWriteFile(t, root, "/app/run.sh", "echo yo")
							e2eChmod(t, root, "/app/run.sh", 0755)
						},
					},
				},
			},
			leftLayers:  []int{0},
			rightLayers: []int{1},
			want: []expectedDiff{
				{path: "/app/run.sh", changeType: ChangeTypeModified, fileType: FileTypeRegular, checkTypes: true},
			},
		},
		{
			name: "later_layer_sees_earlier_delete",
			baseActions: []func(*testing.T, string){
				func(t *testing.T, root string) {
					e2eWriteFile(t, root, "/app/gone.txt", "original")
				},
			},
			layers: []e2eLayer{
				{
					name: "delete",
					actions: []func(*testing.T, string){
						func(t *testing.T, root string) {
							e2eRemove(t, root, "/app/gone.txt")
						},
					},
				},
				{
					name:        "check",
					lowerLayers: []int{0},
					actions: []func(*testing.T, string){
						func(t *testing.T, root string) {
							if _, err := os.Lstat(e2ePath(root, "/app/gone.txt")); !os.IsNotExist(err) {
								t.Errorf("file deleted in layer 0 is visible while building layer 1: err=%v", err)
							}
						},
					},
				},
			},
			leftLayers:  []int{0},
			rightLayers: []int{0, 1},
			want:        nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newE2EWorkspace(t)
			requireOverlayFS(t, w)

			for _, action := range tt.baseActions {
				action(t, w.baseDir)
			}

			for i, layer := range tt.layers {
				w.buildLayer(t, i, layer)
			}

			got := w.runDiff(t, tt.leftLayers, tt.rightLayers)

			assertDiffRecords(t, got, tt.want)
		})
	}
}

// helpers for TestDiffFSOverlayE2E

func requireOverlayFS(t *testing.T, w *e2eWorkspace) {
	t.Helper()

	upper := filepath.Join(w.root, "probe-upper")
	work := filepath.Join(w.root, "probe-work")
	mount := filepath.Join(w.root, "probe-mount")

	for _, dir := range []string{upper, work, mount} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("create probe directory: %v", err)
		}
	}

	options := fmt.Sprintf(
		"lowerdir=%s,upperdir=%s,workdir=%s",
		w.baseDir,
		upper,
		work,
	)

	if err := unix.Mount("overlay", mount, "overlay", 0, options); err != nil {
		t.Skipf("OverlayFS mount not available: %v", err)
	}

	if err := unix.Unmount(mount, 0); err != nil {
		t.Fatalf("unmount OverlayFS probe: %v", err)
	}
}

type e2eLayer struct {
	name        string
	lowerLayers []int
	actions     []func(t *testing.T, root string)
}

type e2eDiffTest struct {
	name        string
	baseActions []func(t *testing.T, root string)
	layers      []e2eLayer
	leftLayers  []int
	rightLayers []int

	want []expectedDiff
}

type e2eWorkspace struct {
	root       string
	baseDir    string
	layerUpper []string
	layerWork  []string
	layerMount []string

	mounted []string
}

func newE2EWorkspace(t *testing.T) *e2eWorkspace {
	t.Helper()

	root := t.TempDir()

	w := &e2eWorkspace{
		root:       root,
		baseDir:    filepath.Join(root, "base"),
		layerUpper: make([]string, 0),
		layerWork:  make([]string, 0),
		layerMount: make([]string, 0),
	}

	if err := os.MkdirAll(w.baseDir, 0755); err != nil {
		t.Fatalf("create base directory: %v", err)
	}

	t.Cleanup(w.cleanup)

	return w
}

func (w *e2eWorkspace) cleanup() {
	for i := len(w.mounted) - 1; i >= 0; i-- {
		if err := unix.Unmount(w.mounted[i], unix.MNT_DETACH); err != nil {
			// Cleanup is best-effort. TempDir cleanup will also run.
			_ = err
		}
	}
}

func (w *e2eWorkspace) mountOverlay(
	t *testing.T,
	mountPoint string,
	lowerDirs []string,
	upperDir string,
	workDir string,
	readOnly bool,
) {
	t.Helper()

	if err := os.MkdirAll(mountPoint, 0755); err != nil {
		t.Fatalf("create mount point %s: %v", mountPoint, err)
	}

	options := fmt.Sprintf("lowerdir=%s", strings.Join(lowerDirs, ":"))

	var flags uintptr

	if !readOnly {
		options += fmt.Sprintf(",upperdir=%s,workdir=%s", upperDir, workDir)
	} else {
		flags |= unix.MS_RDONLY
	}

	if err := unix.Mount("overlay", mountPoint, "overlay", flags, options); err != nil {
		t.Fatalf("mount overlay at %s: %v", mountPoint, err)
	}

	w.mounted = append(w.mounted, mountPoint)
}

func e2ePath(root, containerPath string) string {
	return filepath.Join(
		root,
		filepath.FromSlash(strings.TrimPrefix(containerPath, "/")),
	)
}

func e2eMkdir(t *testing.T, root, path string) {
	t.Helper()

	if err := os.MkdirAll(e2ePath(root, path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func e2eWriteFile(t *testing.T, root, path, content string) {
	t.Helper()

	fullPath := e2ePath(root, path)

	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		t.Fatalf("mkdir parent for %s: %v", path, err)
	}

	if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func e2eChmod(t *testing.T, root, path string, mode os.FileMode) {
	t.Helper()

	if err := os.Chmod(e2ePath(root, path), mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

func e2eRemove(t *testing.T, root, path string) {
	t.Helper()

	if err := os.RemoveAll(e2ePath(root, path)); err != nil {
		t.Fatalf("remove %s: %v", path, err)
	}
}

func e2eRename(t *testing.T, root, oldPath, newPath string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(e2ePath(root, newPath)), 0755); err != nil {
		t.Fatalf("mkdir parent for %s: %v", newPath, err)
	}

	if err := os.Rename(e2ePath(root, oldPath), e2ePath(root, newPath)); err != nil {
		t.Fatalf("rename %s to %s: %v", oldPath, newPath, err)
	}
}

func e2eSymlink(t *testing.T, root, target, linkPath string) {
	t.Helper()

	fullPath := e2ePath(root, linkPath)

	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		t.Fatalf("mkdir parent for symlink %s: %v", linkPath, err)
	}

	if err := os.Symlink(target, fullPath); err != nil {
		t.Fatalf("symlink %s -> %s: %v", linkPath, target, err)
	}
}

func (w *e2eWorkspace) buildLayer(
	t *testing.T,
	index int,
	layer e2eLayer,
) {
	t.Helper()

	upper := filepath.Join(w.root, fmt.Sprintf("layer-%d-upper", index))
	work := filepath.Join(w.root, fmt.Sprintf("layer-%d-work", index))
	mount := filepath.Join(w.root, fmt.Sprintf("layer-%d-mount", index))

	for _, dir := range []string{upper, work, mount} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("create layer directory %s: %v", dir, err)
		}
	}

	// OverlayFS requires upperdir and workdir to be on the same filesystem.
	lowerDirs := make([]string, 0, len(layer.lowerLayers)+1)

	for _, lowerLayerIdx := range layer.lowerLayers {
		if lowerLayerIdx < 0 || lowerLayerIdx >= index {
			t.Fatalf(
				"layer %d has invalid lower layer index %d",
				index,
				lowerLayerIdx,
			)
		}
		lowerDirs = append(lowerDirs, w.layerUpper[lowerLayerIdx])
	}

	// OverlayFS prioritizes the first lower directory.
	// The layer specification is oldest -> newest, so reverse it; the base goes last.
	for i, j := 0, len(lowerDirs)-1; i < j; i, j = i+1, j-1 {
		lowerDirs[i], lowerDirs[j] = lowerDirs[j], lowerDirs[i]
	}
	lowerDirs = append(lowerDirs, w.baseDir)

	w.mountOverlay(t, mount, lowerDirs, upper, work, false)

	// apply fs actions to this layer
	for _, action := range layer.actions {
		action(t, mount)
	}

	if err := unix.Unmount(mount, 0); err != nil {
		t.Fatalf("unmount layer %s: %v", layer.name, err)
	}

	// It was already unmounted; remove it from cleanup tracking.
	for i := len(w.mounted) - 1; i >= 0; i-- {
		if w.mounted[i] == mount {
			w.mounted = append(w.mounted[:i], w.mounted[i+1:]...)
			break
		}
	}

	w.layerUpper = append(w.layerUpper, upper)
	w.layerWork = append(w.layerWork, work)
	w.layerMount = append(w.layerMount, mount)
}

func (w *e2eWorkspace) mergedLowerDirs(layerIDs []int) []string {
	lowerDirs := make([]string, 0, len(layerIDs)+1)

	// Newest layer first.
	for i := len(layerIDs) - 1; i >= 0; i-- {
		lowerDirs = append(lowerDirs, w.layerUpper[layerIDs[i]])
	}

	lowerDirs = append(lowerDirs, w.baseDir)
	return lowerDirs
}

func commonPrefixLen(left, right []int) int {
	i := 0
	for i < len(left) && i < len(right) && left[i] == right[i] {
		i++
	}
	return i
}

func (w *e2eWorkspace) runDiff(
	t *testing.T,
	leftLayers []int,
	rightLayers []int,
) []DiffFileRecord {
	t.Helper()

	leftMount := filepath.Join(w.root, "left-view")
	rightMount := filepath.Join(w.root, "right-view")

	if err := os.MkdirAll(leftMount, 0755); err != nil {
		t.Fatalf("create left mount point: %v", err)
	}
	if err := os.MkdirAll(rightMount, 0755); err != nil {
		t.Fatalf("create right mount point: %v", err)
	}

	w.mountOverlay(
		t,
		leftMount,
		w.mergedLowerDirs(leftLayers),
		"",
		"",
		true,
	)
	w.mountOverlay(
		t,
		rightMount,
		w.mergedLowerDirs(rightLayers),
		"",
		"",
		true,
	)

	leftRoot, err := os.OpenRoot(leftMount)
	if err != nil {
		t.Fatalf("open left root: %v", err)
	}
	defer leftRoot.Close()

	rightRoot, err := os.OpenRoot(rightMount)
	if err != nil {
		t.Fatalf("open right root: %v", err)
	}
	defer rightRoot.Close()

	view := &fsView{
		leftRoot:           leftRoot,
		rightRoot:          rightRoot,
		candidates:         make(map[string]struct{}),
		dirsWithCandidates: make(map[string]struct{}),
	}

	// Collect candidates only from the layers that diverge.
	common := commonPrefixLen(leftLayers, rightLayers)

	unsharedLayers := append([]int{}, leftLayers[common:]...)
	unsharedLayers = append(unsharedLayers, rightLayers[common:]...)

	for _, layerIdx := range unsharedLayers {
		if err := view.collectCandidateDir(w.layerUpper[layerIdx], "/"); err != nil {
			t.Fatalf("collect candidates from layer %d: %v", layerIdx, err)
		}
	}

	var detected []DiffFileRecord

	if err := walkFS(view, "/", &detected); err != nil {
		t.Fatalf("walkFS: %v", err)
	}

	return detected
}

func TestNormalizeTargetDir(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "/"},
		{"/", "/"},
		{"data", "/data"},
		{"/data/", "/data"},
		{"//data//sub", "/data/sub"},
		{"/a/../b", "/b"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := normalizeTargetDir(tc.in)
			if err != nil || got != tc.want {
				t.Fatalf("normalizeTargetDir(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
			}
		})
	}
	for _, in := range []string{"..", "../x", "/../x", "a/../../b"} {
		t.Run(in, func(t *testing.T) {
			if got, err := normalizeTargetDir(in); err == nil {
				t.Fatalf("normalizeTargetDir(%q) = %q, want an error", in, got)
			}
		})
	}
}

func TestComputeCkptsDiffIDDistinguishesPairs(t *testing.T) {
	ids := map[string]string{}
	for _, pair := range [][2]string{{"A", "B"}, {"B", "A"}, {"ab", "c"}, {"a", "bc"}} {
		id := computeCkptsDiffID(pair[0], pair[1])
		if prev, ok := ids[id]; ok {
			t.Fatalf("computeCkptsDiffID(%q, %q) = %s, same as %s", pair[0], pair[1], id, prev)
		}
		ids[id] = pair[0] + "/" + pair[1]
	}
}

func TestComputeResultPathSkipsOldFormatResults(t *testing.T) {
	dir := "/s/diffs/A-B-0123"
	key, err := computeTargetHash("/data")
	if err != nil {
		t.Fatal(err)
	}
	got, err := computeResultPath(dir, "/data")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, fmt.Sprintf("result-v%d-%s.json", diffResultVersion, key)); got != want {
		t.Fatalf("computeResultPath = %s, want %s", got, want)
	}
	// Results written before the name carried a version.
	if old := filepath.Join(dir, "result-"+key+".json"); got == old {
		t.Fatalf("computeResultPath = %s, the name of an unversioned result", got)
	}
}

// newDiffWorkspaceManager returns a manager with ready checkpoints A and B and
// the result and workspace paths of diff A B on /.
func newDiffWorkspaceManager(t *testing.T) (m *Manager, resultPath, workspaceDir string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("removing a diff workspace unmounts its views, which requires root")
	}
	base := t.TempDir()
	m = &Manager{baseDir: base, metadataDir: filepath.Join(base, "metadata"), originalDir: filepath.Join(base, "original")}
	for _, id := range []string{"A", "B"} {
		if err := m.saveMetadata(id, Metadata{ID: id, Status: CheckpointStatusReady}); err != nil {
			t.Fatalf("saveMetadata(%s): %v", id, err)
		}
	}
	pairDir := m.diffDir(computeCkptsDiffID("A", "B"))
	resultPath, err := computeResultPath(pairDir, "/")
	if err != nil {
		t.Fatal(err)
	}
	workspaceDir, err = computeTargetTempDir(pairDir, "/")
	if err != nil {
		t.Fatal(err)
	}
	return m, resultPath, workspaceDir
}

func TestClaimDiffWorkspaceOneOwnerAtATime(t *testing.T) {
	m, resultPath, workspaceDir := newDiffWorkspaceManager(t)

	owner, cached, running, err := m.claimDiffWorkspace("A", "B", resultPath, workspaceDir)
	if err != nil || owner == nil || cached != nil || running != nil {
		t.Fatalf("first claim = (%v, %q, %v, %v), want a workspace", owner, cached, running, err)
	}
	for _, dir := range []string{owner.leftMountPoint, owner.rightMountPoint} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("mount point %s not created: %v", dir, err)
		}
	}

	second, cached, running, err := m.claimDiffWorkspace("A", "B", resultPath, workspaceDir)
	if err != nil || second != nil || cached != nil || running == nil {
		t.Fatalf("claim during a running diff = (%v, %q, %v, %v), want the running workspace", second, cached, running, err)
	}
	if _, err := os.Stat(workspaceDir); err != nil {
		t.Fatalf("the running diff's workspace was removed: %v", err)
	}

	// A waiter gets the shared lock once the owner is done.
	waited := make(chan error, 1)
	go func() { waited <- unix.Flock(int(running.Fd()), unix.LOCK_SH) }()
	select {
	case err := <-waited:
		t.Fatalf("waiter got the lock (err=%v) while the diff was running", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := atomicWriteFile(resultPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	owner.close()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("waiter: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("waiter still blocked after the owner finished")
	}
	running.Close()
	if _, err := os.Stat(workspaceDir); !os.IsNotExist(err) {
		t.Fatalf("workspace still present after close: err=%v", err)
	}

	third, cached, running, err := m.claimDiffWorkspace("A", "B", resultPath, workspaceDir)
	if err != nil || third != nil || string(cached) != "{}" || running != nil {
		t.Fatalf("claim after the diff finished = (%v, %q, %v, %v), want the cached result", third, cached, running, err)
	}
}

// A result file cut short by a crash is not served; the diff runs again.
func TestClaimDiffWorkspaceIgnoresIncompleteResult(t *testing.T) {
	m, resultPath, workspaceDir := newDiffWorkspaceManager(t)
	for _, content := range []string{"", `{"from": "A", "to"`, "\x00\x00\x00\x00"} {
		if err := os.MkdirAll(filepath.Dir(resultPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(resultPath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		d, cached, running, err := m.claimDiffWorkspace("A", "B", resultPath, workspaceDir)
		if err != nil || d == nil || cached != nil || running != nil {
			t.Fatalf("claim next to result %q = (%v, %q, %v, %v), want a new workspace", content, d, cached, running, err)
		}
		if err := writeDiffResult(resultPath, []byte(`{"from": "A"}`)); err != nil {
			t.Fatalf("writeDiffResult: %v", err)
		}
		d.close()
		_, cached, _, err = m.claimDiffWorkspace("A", "B", resultPath, workspaceDir)
		if err != nil || string(cached) != `{"from": "A"}` {
			t.Fatalf("claim after rewriting result %q = (%q, %v), want the new result", content, cached, err)
		}
		if _, err := os.Stat(resultPath + ".tmp"); !os.IsNotExist(err) {
			t.Errorf("temporary result file left behind: err=%v", err)
		}
	}
}

func TestClaimDiffWorkspaceReplacesInterruptedRun(t *testing.T) {
	m, resultPath, workspaceDir := newDiffWorkspaceManager(t)

	// What a killed diff leaves: the workspace without a lock holder or a result.
	leftover := filepath.Join(workspaceDir, diffLeftMount, "leftover")
	if err := os.MkdirAll(leftover, 0o755); err != nil {
		t.Fatal(err)
	}

	d, cached, running, err := m.claimDiffWorkspace("A", "B", resultPath, workspaceDir)
	if err != nil || d == nil || cached != nil || running != nil {
		t.Fatalf("claim = (%v, %q, %v, %v), want a new workspace", d, cached, running, err)
	}
	defer d.close()
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Fatalf("content of the interrupted workspace survived: err=%v", err)
	}
	f, busy, err := lockDiffWorkspace(workspaceDir)
	if err != nil || !busy {
		t.Fatalf("new workspace is not locked: busy=%v err=%v", busy, err)
	}
	f.Close()
}

func TestClaimDiffWorkspaceRefusesCheckpointNotReady(t *testing.T) {
	m, resultPath, workspaceDir := newDiffWorkspaceManager(t)
	if err := m.saveMetadata("B", Metadata{ID: "B", Status: CheckpointStatusFailed}); err != nil {
		t.Fatal(err)
	}
	if d, _, _, err := m.claimDiffWorkspace("A", "B", resultPath, workspaceDir); err == nil {
		d.close()
		t.Fatal("claim succeeded on a failed checkpoint")
	}
	if _, err := os.Stat(workspaceDir); !os.IsNotExist(err) {
		t.Fatalf("workspace created for a refused diff: err=%v", err)
	}
}

func TestExplorePsFlatReadsEveryProcess(t *testing.T) {
	dir := t.TempDir()
	writeProcImages(t, dir, []ProcInfo{
		{PID: 1, PPID: 0, PGID: 1, SID: 1, Comm: "bash_init"},
		{PID: 11, PPID: 1, PGID: 11, SID: 11, Comm: "bash"},
		{PID: 12, PPID: 11, PGID: 12, SID: 11, Comm: "sleep"},
	})

	got, err := explorePsFlat(dir)
	if err != nil {
		t.Fatalf("explorePsFlat: %v", err)
	}
	if pids := sortedPIDs(got); !reflect.DeepEqual(pids, []uint32{1, 11, 12}) {
		t.Fatalf("explorePsFlat pids = %v, want [1 11 12]", pids)
	}
	if got[12].Comm != "sleep" || got[12].PPID != 11 {
		t.Fatalf("explorePsFlat[12] = %+v, want comm sleep and ppid 11", got[12])
	}
}

func TestExplorePsFlatFailsWithoutCoreImage(t *testing.T) {
	dir := t.TempDir()
	writeProcImages(t, dir, []ProcInfo{{PID: 1, Comm: "bash_init"}, {PID: 12, PPID: 1, Comm: "sleep"}})
	if err := os.Remove(filepath.Join(dir, "core-12.img")); err != nil {
		t.Fatal(err)
	}
	if _, err := explorePsFlat(dir); err == nil {
		t.Fatal("explorePsFlat succeeded without core-12.img")
	}
}

func TestGetProcDiffReportsChangedPIDs(t *testing.T) {
	m := &Manager{baseDir: t.TempDir()}
	writeProcImages(t, m.checkpointCriuDir("A"), []ProcInfo{
		{PID: 1, PPID: 0, PGID: 1, SID: 1, Comm: "bash_init"},
		{PID: 11, PPID: 1, PGID: 11, SID: 11, Comm: "bash"},
		{PID: 12, PPID: 11, PGID: 12, SID: 11, Comm: "sleep"},
		{PID: 13, PPID: 11, PGID: 13, SID: 11, Comm: "sh"},
	})
	writeProcImages(t, m.checkpointCriuDir("B"), []ProcInfo{
		{PID: 1, PPID: 0, PGID: 1, SID: 1, Comm: "bash_init"},
		{PID: 11, PPID: 1, PGID: 11, SID: 11, Comm: "bash"},
		{PID: 13, PPID: 11, PGID: 13, SID: 11, Comm: "python3"},
		{PID: 14, PPID: 11, PGID: 14, SID: 11, Comm: "sleep"},
	})

	want := map[uint32]ChangeType{12: ChangeTypeDeleted, 13: ChangeTypeModified, 14: ChangeTypeAdded}
	if got := procDiffByPID(t, m, "A", "B"); !reflect.DeepEqual(got, want) {
		t.Fatalf("getProcDiff(A, B) = %v, want %v", got, want)
	}
	want = map[uint32]ChangeType{12: ChangeTypeAdded, 13: ChangeTypeModified, 14: ChangeTypeDeleted}
	if got := procDiffByPID(t, m, "B", "A"); !reflect.DeepEqual(got, want) {
		t.Fatalf("getProcDiff(B, A) = %v, want %v", got, want)
	}
}

func TestGetProcDiffSameProcessesIsEmpty(t *testing.T) {
	m := &Manager{baseDir: t.TempDir()}
	procs := []ProcInfo{
		{PID: 1, PPID: 0, PGID: 1, SID: 1, Comm: "bash_init"},
		{PID: 11, PPID: 1, PGID: 11, SID: 11, Comm: "bash"},
	}
	writeProcImages(t, m.checkpointCriuDir("A"), procs)
	writeProcImages(t, m.checkpointCriuDir("B"), procs)
	if got := procDiffByPID(t, m, "A", "B"); len(got) != 0 {
		t.Fatalf("getProcDiff(A, B) = %v, want no records", got)
	}
}

func TestGetProcDiffWaitsForImagesLock(t *testing.T) {
	m := &Manager{baseDir: t.TempDir()}
	procs := []ProcInfo{{PID: 1, PPID: 0, PGID: 1, SID: 1, Comm: "bash_init"}}
	writeProcImages(t, m.checkpointCriuDir("A"), procs)
	writeProcImages(t, m.checkpointCriuDir("B"), procs)

	// A tmpfs flush holds this lock exclusively while it repoints the images.
	unlock, err := lockImages(m.checkpointDir("B"), true)
	if err != nil {
		t.Fatalf("lockImages: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		var records []DiffProcRecord
		done <- m.getProcDiff("A", "B", &records)
	}()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("getProcDiff returned (err=%v) while a flush held the images lock", err)
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("getProcDiff: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("getProcDiff still blocked after the images lock was released")
	}

	for _, id := range []string{"A", "B"} {
		if _, err := os.Stat(imagesLockPath(m.checkpointCriuDir(id))); !os.IsNotExist(err) {
			t.Errorf("lock file created inside the images dir of %s: err=%v", id, err)
		}
	}
}

// procDiffByPID runs getProcDiff and returns the change type reported for
// each pid.
func procDiffByPID(t *testing.T, m *Manager, left, right string) map[uint32]ChangeType {
	t.Helper()
	var records []DiffProcRecord
	if err := m.getProcDiff(left, right, &records); err != nil {
		t.Fatalf("getProcDiff(%s, %s): %v", left, right, err)
	}
	got := map[uint32]ChangeType{}
	for _, r := range records {
		if _, dup := got[r.PID]; dup {
			t.Fatalf("getProcDiff(%s, %s) reports pid %d twice", left, right, r.PID)
		}
		got[r.PID] = r.ChangeType
	}
	return got
}

func sortedPIDs(procs map[uint32]ProcInfo) []uint32 {
	pids := make([]uint32, 0, len(procs))
	for pid := range procs {
		pids = append(pids, pid)
	}
	sort.Slice(pids, func(i, j int) bool { return pids[i] < pids[j] })
	return pids
}

// writeProcImages writes pstree.img and one core-<pid>.img per process into
// dir, with the fields explorePsFlat reads.
func writeProcImages(t *testing.T, dir string, procs []ProcInfo) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	tree := &crit.CriuImage{Magic: "PSTREE"}
	for _, p := range procs {
		pid, ppid, pgid, sid := p.PID, p.PPID, p.PGID, p.SID
		tree.Entries = append(tree.Entries, &crit.CriuEntry{Message: &pstree.PstreeEntry{
			Pid: &pid, Ppid: &ppid, Pgid: &pgid, Sid: &sid,
		}})

		march := criu_core.CoreEntry_X86_64
		var zero32 uint32
		var zero64 uint64
		comm := p.Comm
		core := &crit.CriuImage{Magic: "CORE", Entries: []*crit.CriuEntry{{Message: &criu_core.CoreEntry{
			Mtype: &march,
			Tc: &criu_core.TaskCoreEntry{
				TaskState: &zero32, ExitCode: &zero32, Personality: &zero32,
				Flags: &zero32, BlkSigset: &zero64, Comm: &comm,
			},
		}}}}
		writeCritImage(t, filepath.Join(dir, fmt.Sprintf("core-%d.img", p.PID)), core)
	}
	writeCritImage(t, filepath.Join(dir, "pstree.img"), tree)
}

func writeCritImage(t *testing.T, path string, img *crit.CriuImage) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := crit.New(nil, f, "", false, false).Encode(img); err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
}
