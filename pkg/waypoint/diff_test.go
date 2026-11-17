package waypoint

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/checkpoint-restore/go-criu/v8/crit"
	criu_core "github.com/checkpoint-restore/go-criu/v8/crit/images/criu-core"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/pstree"
	"golang.org/x/sys/unix"
)

func TestWalkFSReportsFileChanges(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	for _, root := range []string{left, right} {
		writeCopyTestFile(t, filepath.Join(root, "same.txt"), "same", 0o644)
		writeCopyTestFile(t, filepath.Join(root, "sub", "keep"), "keep", 0o644)
		if err := unix.Mkfifo(filepath.Join(root, "fifo"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeCopyTestFile(t, filepath.Join(left, "mod.txt"), "v1", 0o644)
	writeCopyTestFile(t, filepath.Join(right, "mod.txt"), "v2", 0o644)
	writeCopyTestFile(t, filepath.Join(left, "del.txt"), "deleted", 0o644)
	writeCopyTestFile(t, filepath.Join(right, "new.txt"), "added", 0o644)
	writeCopyTestFile(t, filepath.Join(right, "sub", "new"), "added", 0o644)
	writeCopyTestFile(t, filepath.Join(left, "chmod.txt"), "c", 0o644)
	writeCopyTestFile(t, filepath.Join(right, "chmod.txt"), "c", 0o644)
	if err := os.Chmod(filepath.Join(right, "chmod.txt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("same.txt", filepath.Join(left, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("mod.txt", filepath.Join(right, "link")); err != nil {
		t.Fatal(err)
	}

	got := walkDiff(t, left, right, "/")
	want := []DiffFileRecord{
		{Path: "/chmod.txt", ChangeType: ChangeTypeModified, FileType: FileTypeRegular},
		{Path: "/del.txt", ChangeType: ChangeTypeDeleted, FileType: FileTypeRegular},
		{Path: "/link", ChangeType: ChangeTypeModified, FileType: FileTypeSymlink},
		{Path: "/mod.txt", ChangeType: ChangeTypeModified, FileType: FileTypeRegular},
		{Path: "/new.txt", ChangeType: ChangeTypeAdded, FileType: FileTypeRegular},
		{Path: "/sub/new", ChangeType: ChangeTypeAdded, FileType: FileTypeRegular},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("walkFS records:\n got %+v\nwant %+v", got, want)
	}
}

func TestWalkFSIgnoresSameContent(t *testing.T) {
	// Files written separately (different inodes and mtimes) with the same
	// content and mode are equal.
	left, right := t.TempDir(), t.TempDir()
	for _, root := range []string{left, right} {
		writeCopyTestFile(t, filepath.Join(root, "a.txt"), "same", 0o644)
		writeCopyTestFile(t, filepath.Join(root, "dir", "b.txt"), "same too", 0o644)
		if err := os.Symlink("a.txt", filepath.Join(root, "link")); err != nil {
			t.Fatal(err)
		}
	}
	if got := walkDiff(t, left, right, "/"); len(got) != 0 {
		t.Fatalf("walkFS records = %+v, want none", got)
	}
}

func TestWalkFSReportsFilesUnderAddedAndDeletedDirectories(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	writeCopyTestFile(t, filepath.Join(left, "gone", "a"), "a", 0o644)
	writeCopyTestFile(t, filepath.Join(left, "gone", "deep", "b"), "b", 0o644)
	writeCopyTestFile(t, filepath.Join(right, "added", "deep", "c"), "c", 0o644)

	got := map[string]ChangeType{}
	for _, r := range walkDiff(t, left, right, "/") {
		got[r.Path] = r.ChangeType
	}
	for path, want := range map[string]ChangeType{
		"/gone/a":       ChangeTypeDeleted,
		"/gone/deep/b":  ChangeTypeDeleted,
		"/added/deep/c": ChangeTypeAdded,
	} {
		if got[path] != want {
			t.Errorf("record for %s = %q, want %q (all: %v)", path, got[path], want, got)
		}
	}
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

// walkDiff runs walkFS and returns its records sorted by path, as PrintDiff
// does.
func walkDiff(t *testing.T, left, right, containerDir string) []DiffFileRecord {
	t.Helper()
	records := []DiffFileRecord{}
	leftRoot, err := os.OpenRoot(left)
	if err != nil {
		t.Fatalf("os.OpenRoot(%q): %v", left, err)
	}
	defer leftRoot.Close()
	rightRoot, err := os.OpenRoot(right)
	if err != nil {
		t.Fatalf("os.OpenRoot(%q): %v", right, err)
	}
	defer rightRoot.Close()
	targetForRoot, err := cleanRootPath(containerDir) // dependency on copy.go
	if err != nil {
		t.Fatalf("cleanRootPath(%q): %v", targetForRoot, err)
	}

	if err := walkFS(leftRoot, rightRoot, targetForRoot, containerDir, &records); err != nil {
		t.Fatalf("walkFS: %v", err)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Path < records[j].Path })
	return records
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
