package waypoint

// TODO add description

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/checkpoint-restore/go-criu/v8/crit"
	criu_core "github.com/checkpoint-restore/go-criu/v8/crit/images/criu-core"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/pstree"
	"golang.org/x/sys/unix"
)

type DiffResult struct {
	From       string           `json:"from"`
	To         string           `json:"to"`
	Filesystem FilesystemDiff   `json:"filesystem"`
	Processes  []DiffProcRecord `json:"processes"`
}

type FilesystemDiff struct {
	TargetDir string           `json:"target_dir"`
	Records   []DiffFileRecord `json:"records"`
}

type ChangeType string

const (
	ChangeTypeAdded    ChangeType = "added"
	ChangeTypeDeleted  ChangeType = "deleted"
	ChangeTypeModified ChangeType = "modified"
)

type DiffFileRecord struct {
	Path       string     `json:"path"`
	ChangeType ChangeType `json:"change_type"`
	FileType   FileType   `json:"file_type"`
	// TODO: show the actual line by line diff like blame
	FromFileType FileType `json:"from_file_type,omitempty"`
	ToFileType   FileType `json:"to_file_type,omitempty"`
}

type FileType string

// types currently tracked
const (
	FileTypeSymlink  FileType = "symlink"
	FileTypeRegular  FileType = "regular"
	FileTypeDevice   FileType = "device"    // modified = major/minor number change
	FileTypeFIFO     FileType = "fifo"      // currently only add/delete tracked
	FileTypeSocket   FileType = "socket"    // currently only add/delete tracked
	FileTypeDir      FileType = "directory" // only tracked if type change
	FileTypeOther    FileType = "other"
	FileTypeModified FileType = "modified"
)

type DiffProcRecord struct {
	PID        uint32       `json:"pid"`
	ChangeType ChangeType   `json:"change_type"`
	Process    *ProcInfo    `json:"process,omitempty"`
	Changes    []ProcChange `json:"changes,omitempty"`
}

type ProcChange struct {
	Field    string `json:"field"`
	Previous any    `json:"previous"`
	New      any    `json:"new"`
}

type ProcInfo struct {
	PID  uint32 `json:"pid"`
	PGID uint32 `json:"pgid"`
	PPID uint32 `json:"ppid"`
	SID  uint32 `json:"sid"`
	Comm string `json:"comm"`
}

type DiffFS struct {
	overlayDir string
	lock       *os.File // overlayDir, flocked while the diff runs

	leftOriginalDir  string
	rightOriginalDir string

	leftLayerIDs  []string
	rightLayerIDs []string

	leftMountPoint  string
	rightMountPoint string
}

type fsView struct {
	leftRoot               *os.Root
	rightRoot              *os.Root
	containerViewTargetDir string
	candidates             map[string]struct{} // non directories in unshared layers
	dirsWithCandidates     map[string]struct{} // directories in unshared layers
}

type side string

const (
	LeftSide  side = "left"
	RightSide side = "right"
)

func (view *fsView) getRootSide(s side) (*os.Root, error) {
	switch s {
	case LeftSide:
		return view.leftRoot, nil
	case RightSide:
		return view.rightRoot, nil
	default:
		return nil, fmt.Errorf("invalid filesystem side: %q", s)
	}
}

// compareBufs are the read buffers of a file comparison, one per side.
type compareBufs struct {
	left, right []byte
}

func newCompareBufs() *compareBufs {
	const chunkSize = 32 * 1024
	return &compareBufs{left: make([]byte, chunkSize), right: make([]byte, chunkSize)}
}

func areRegFilesDiff(leftRoot, rightRoot *os.Root, path string, bufs *compareBufs) (bool, error) {
	leftFile, err := leftRoot.Open(path)
	if err != nil {
		return false, fmt.Errorf("open left file %s: %w", path, err)
	}
	defer leftFile.Close()

	rightFile, err := rightRoot.Open(path)
	if err != nil {
		return false, fmt.Errorf("open right file %s: %w", path, err)
	}
	defer rightFile.Close()

	// Compare sizes first.
	leftInfo, err := leftFile.Stat()
	if err != nil {
		return false, fmt.Errorf("stat left file %s: %w", path, err)
	}
	rightInfo, err := rightFile.Stat()
	if err != nil {
		return false, fmt.Errorf("stat right file %s: %w", path, err)
	}
	if leftInfo.Size() != rightInfo.Size() {
		return true, nil
	}

	// Compare contents in chunks.
	leftBuf, rightBuf := bufs.left, bufs.right

	for {
		leftN, leftErr := io.ReadFull(leftFile, leftBuf)
		rightN, rightErr := io.ReadFull(rightFile, rightBuf)

		if !bytes.Equal(leftBuf[:leftN], rightBuf[:rightN]) {
			return true, nil
		}

		if leftErr == nil && rightErr == nil {
			continue
		}
		if (leftErr == io.EOF || leftErr == io.ErrUnexpectedEOF) &&
			(rightErr == io.EOF || rightErr == io.ErrUnexpectedEOF) {
			return false, nil
		}

		if leftErr == io.EOF || leftErr == io.ErrUnexpectedEOF ||
			rightErr == io.EOF || rightErr == io.ErrUnexpectedEOF {
			return true, nil
		}

		if leftErr != nil {
			return false, fmt.Errorf("read left file %s: %w", path, leftErr)
		}
		return false, fmt.Errorf("read right file %s: %w", path, rightErr)
	}
}

func getFileType(mode fs.FileMode) FileType {
	if mode.IsRegular() {
		return FileTypeRegular
	}
	if mode&os.ModeSymlink != 0 {
		return FileTypeSymlink
	}
	if mode&os.ModeDevice != 0 {
		return FileTypeDevice
	}
	if mode&os.ModeNamedPipe != 0 {
		return FileTypeFIFO
	}
	if mode&os.ModeSocket != 0 {
		return FileTypeSocket
	}
	return FileTypeOther
}

func processNonDirMod(view *fsView, containerViewPath string, bufs *compareBufs, detected *[]DiffFileRecord) error {
	rootViewPath, err := cleanRootPath(containerViewPath)
	if err != nil {
		return err
	}
	leftRoot := view.leftRoot
	rightRoot := view.rightRoot
	leftInfo, err := leftRoot.Lstat(rootViewPath)
	if err != nil {
		return fmt.Errorf("lstat %s: %w", rootViewPath, err)
	}
	rightInfo, err := rightRoot.Lstat(rootViewPath)
	if err != nil {
		return fmt.Errorf("lstat %s: %w", rootViewPath, err)
	}
	leftFileType := getFileType(leftInfo.Mode())
	rightFileType := getFileType(rightInfo.Mode())
	// Mode change
	if leftInfo.Mode() != rightInfo.Mode() {
		if leftFileType == rightFileType {
			*detected = append(*detected, DiffFileRecord{
				Path:       containerViewPath,
				ChangeType: ChangeTypeModified,
				FileType:   leftFileType,
			})
		} else {
			*detected = append(*detected, DiffFileRecord{
				Path:         containerViewPath,
				ChangeType:   ChangeTypeModified,
				FileType:     FileTypeModified,
				FromFileType: leftFileType,
				ToFileType:   rightFileType,
			})
		}
		return nil
	}
	// Other changes currently tracked: cmp on regular, symlink target, dev major/minor. TODO support other changes/types
	mod := false
	if leftFileType == FileTypeRegular {
		res, err := areRegFilesDiff(leftRoot, rightRoot, rootViewPath, bufs)
		if err != nil {
			return err
		}
		mod = res
	} else if leftFileType == FileTypeSymlink {
		leftTarget, err := leftRoot.Readlink(rootViewPath)
		if err != nil {
			return fmt.Errorf("readlink %s: %w", rootViewPath, err)
		}
		rightTarget, err := rightRoot.Readlink(rootViewPath)
		if err != nil {
			return fmt.Errorf("readlink %s: %w", rootViewPath, err)
		}
		mod = (leftTarget != rightTarget)
	} else if leftFileType == FileTypeDevice {
		leftSys, ok := leftInfo.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("unexpected stat type for %s", rootViewPath)
		}
		rightSys, ok := rightInfo.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("unexpected stat type for %s", rootViewPath)
		}
		leftDev := uint64(leftSys.Rdev)
		rightDev := uint64(rightSys.Rdev)
		mod = (unix.Major(leftDev) != unix.Major(rightDev) ||
			unix.Minor(leftDev) != unix.Minor(rightDev))
	}
	if mod {
		*detected = append(*detected, DiffFileRecord{
			Path:       containerViewPath,
			ChangeType: ChangeTypeModified,
			FileType:   leftFileType,
		})
	}
	return nil
}

// recurseSideChangeType reports the entry at containerViewPath, and everything
// below it, as changeType. mode is the entry's type from its directory listing.
func recurseSideChangeType(view *fsView, s side, containerViewPath string, mode fs.FileMode, detected *[]DiffFileRecord, changeType ChangeType) error {
	if !mode.IsDir() {
		*detected = append(*detected, DiffFileRecord{
			Path:       containerViewPath,
			ChangeType: changeType,
			FileType:   getFileType(mode),
		})
		return nil
	}

	rootViewPath, err := cleanRootPath(containerViewPath)
	if err != nil {
		return err
	}
	root, err := view.getRootSide(s)
	if err != nil {
		return err
	}
	entries, err := listDir(root, rootViewPath)
	if err != nil {
		return err
	}
	for name, entryMode := range entries {
		containerViewSubpath := filepath.Join(containerViewPath, name)
		if err := recurseSideChangeType(view, s, containerViewSubpath, entryMode, detected, changeType); err != nil {
			return err
		}
	}
	return nil
}

// listDir returns the type bits of each entry of a directory in root. ReadDir
// on a file opened in a Root stats every entry, so the listing reads a
// duplicate of the descriptor, which takes the types from the directory itself.
func listDir(root *os.Root, rootViewDir string) (map[string]fs.FileMode, error) {
	dir, err := root.Open(rootViewDir)
	if err != nil {
		return nil, fmt.Errorf("open directory %s: %w", rootViewDir, err)
	}
	defer dir.Close()
	fd, err := unix.FcntlInt(dir.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("duplicate descriptor of directory %s: %w", rootViewDir, err)
	}
	plain := os.NewFile(uintptr(fd), dir.Name())
	defer plain.Close()
	dentries, err := plain.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("read directory %s: %w", rootViewDir, err)
	}
	entries := make(map[string]fs.FileMode, len(dentries))
	for _, dentry := range dentries {
		entries[dentry.Name()] = dentry.Type()
	}
	return entries, nil
}

// compareWorkers is the number of goroutines that compare candidate files.
var compareWorkers = min(runtime.GOMAXPROCS(0), 8)

// compareQueueLen is the number of candidate paths the walk can queue before
// it waits for the workers.
const compareQueueLen = 4096

// comparePool compares candidate files on compareWorkers goroutines while the
// directory walk goes on.
type comparePool struct {
	view *fsView
	jobs chan string
	wg   sync.WaitGroup

	failed  atomic.Bool
	mu      sync.Mutex
	err     error // first error of the walk or of a worker
	records []DiffFileRecord
}

func newComparePool(view *fsView) *comparePool {
	p := &comparePool{view: view, jobs: make(chan string, compareQueueLen)}
	for i := 0; i < compareWorkers; i++ {
		p.wg.Add(1)
		go p.work()
	}
	return p
}

func (p *comparePool) work() {
	defer p.wg.Done()
	var bufs *compareBufs
	var records []DiffFileRecord
	for path := range p.jobs {
		if p.failed.Load() {
			continue // drain the queue
		}
		if bufs == nil {
			bufs = newCompareBufs()
		}
		if err := processNonDirMod(p.view, path, bufs, &records); err != nil {
			p.fail(err)
		}
	}
	p.mu.Lock()
	p.records = append(p.records, records...)
	p.mu.Unlock()
}

// fail records the first error; workers skip the jobs still queued.
func (p *comparePool) fail(err error) {
	p.mu.Lock()
	if p.err == nil {
		p.err = err
	}
	p.mu.Unlock()
	p.failed.Store(true)
}

// submit queues a candidate path, waiting while the queue is full. After a
// failure it returns the first error instead.
func (p *comparePool) submit(containerViewPath string) error {
	if p.failed.Load() {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.err
	}
	p.jobs <- containerViewPath
	return nil
}

// wait closes the queue and returns the records of all workers once they are done.
func (p *comparePool) wait() ([]DiffFileRecord, error) {
	close(p.jobs)
	p.wg.Wait()
	return p.records, p.err
}

// walkFS appends the differences below containerViewDir to detected, in no
// particular order.
func walkFS(view *fsView, containerViewDir string, detected *[]DiffFileRecord) error {
	pool := newComparePool(view)
	if err := walkDir(view, pool, containerViewDir, detected); err != nil {
		pool.fail(err)
	}
	compared, err := pool.wait()
	if err != nil {
		return err
	}
	*detected = append(*detected, compared...)
	return nil
}

func walkDir(view *fsView, pool *comparePool, containerViewDir string, detected *[]DiffFileRecord) error {
	// note: won't follow symlink dirs
	rootViewDir, err := cleanRootPath(containerViewDir)
	if err != nil {
		return err
	}
	leftEntries, err := listDir(view.leftRoot, rootViewDir)
	if err != nil {
		return fmt.Errorf("left side: %w", err)
	}
	rightEntries, err := listDir(view.rightRoot, rootViewDir)
	if err != nil {
		return fmt.Errorf("right side: %w", err)
	}

	for name, leftMode := range leftEntries {
		containerViewSubpath := filepath.Join(containerViewDir, name)
		rightMode, exists := rightEntries[name]
		if !exists {
			// Left-only entry -> deleted
			if err := recurseSideChangeType(view, LeftSide, containerViewSubpath, leftMode, detected, ChangeTypeDeleted); err != nil {
				return err
			}
			continue
		}
		if leftMode.IsDir() && rightMode.IsDir() {
			_, exists := view.dirsWithCandidates[containerViewSubpath]
			if exists {
				// both directories and present in an unshared layer -> recurse
				if err := walkDir(view, pool, containerViewSubpath, detected); err != nil {
					return err
				}
			}
			continue
		}
		// Just one side is directory
		if leftMode.IsDir() != rightMode.IsDir() {
			var fromFileType, toFileType FileType
			if leftMode.IsDir() {
				fromFileType = FileTypeDir
				toFileType = getFileType(rightMode)
			} else {
				fromFileType = getFileType(leftMode)
				toFileType = FileTypeDir
			}
			*detected = append(*detected, DiffFileRecord{
				Path:         containerViewSubpath,
				ChangeType:   ChangeTypeModified,
				FileType:     FileTypeModified,
				FromFileType: fromFileType,
				ToFileType:   toFileType,
			})
			// collect directory side descendents
			if leftMode.IsDir() {
				if err := recurseSideChangeType(view, LeftSide, containerViewSubpath, fs.ModeDir, detected, ChangeTypeDeleted); err != nil {
					return err
				}
			} else {
				if err := recurseSideChangeType(view, RightSide, containerViewSubpath, fs.ModeDir, detected, ChangeTypeAdded); err != nil {
					return err
				}
			}
			continue
		}
		_, exists = view.candidates[containerViewSubpath]
		if exists {
			// Both non-directories and candidates -> check for modification
			if err := pool.submit(containerViewSubpath); err != nil {
				return err
			}
		}
	}
	// Right-only entries -> added
	for name, rightMode := range rightEntries {
		if _, exists := leftEntries[name]; exists {
			continue
		}
		containerViewSubpath := filepath.Join(containerViewDir, name)
		if err := recurseSideChangeType(view, RightSide, containerViewSubpath, rightMode, detected, ChangeTypeAdded); err != nil {
			return err
		}
		continue
	}
	return nil
}

// collectCandidateDir records one unshared layer: non-directories are candidates,
// directories are walked. A directory absent from every unshared layer is the
// same on both sides and is skipped.
func (view *fsView) collectCandidateDir(hostDirPath, containerDirPath string) error {
	dentries, err := os.ReadDir(hostDirPath)
	if err != nil {
		return fmt.Errorf("read directory %s: %w", hostDirPath, err)
	}
	for _, dentry := range dentries {
		hostDentryPath := filepath.Join(hostDirPath, dentry.Name())
		containerDentryPath := filepath.Join(containerDirPath, dentry.Name())
		if dentry.IsDir() {
			view.dirsWithCandidates[containerDentryPath] = struct{}{}
			if err := view.collectCandidateDir(hostDentryPath, containerDentryPath); err != nil {
				return err
			}
		} else {
			view.candidates[containerDentryPath] = struct{}{}
		}
	}
	return nil
}

func (m *Manager) getAllCandidates(d *DiffFS, view *fsView) error {
	// candidates contains non-directory paths from the unshared layers.
	// dirsWithCandidates contains the directories of the unshared layers; only these are traversed.
	view.candidates = make(map[string]struct{})
	view.dirsWithCandidates = make(map[string]struct{})
	lastCommonIdx := -1
	leftLayers := d.leftLayerIDs
	rightLayers := d.rightLayerIDs
	if filepath.Clean(d.leftOriginalDir) != filepath.Clean(d.rightOriginalDir) {
		// collect candidates from original dirs and all ckpts
		if err := view.collectCandidateDir(d.leftOriginalDir, "/"); err != nil {
			return err
		}
		if err := view.collectCandidateDir(d.rightOriginalDir, "/"); err != nil {
			return err
		}
	} else {
		// only look at upper layers from unshared ckpts
		for i := 0; i < len(leftLayers) && i < len(rightLayers); i++ {
			if leftLayers[i] != rightLayers[i] {
				break
			}
			lastCommonIdx = i
		}
	}

	unsharedCkpts := append([]string{}, leftLayers[lastCommonIdx+1:]...)
	unsharedCkpts = append(unsharedCkpts, rightLayers[lastCommonIdx+1:]...)
	for _, ckptID := range unsharedCkpts {
		upperDirPath := m.checkpointUpperDir(ckptID)
		if err := view.collectCandidateDir(upperDirPath, "/"); err != nil {
			return err
		}
	}
	return nil
}

func existsTargetDir(view *fsView, s side) (bool, error) {
	var root *os.Root
	root, err := view.getRootSide(s)
	if err != nil {
		return false, err
	}
	path, err := cleanRootPath(view.containerViewTargetDir)
	if err != nil {
		return false, err
	}
	info, err := root.Lstat(path) // requires nonsymlink dir
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("lstat %s: %w", path, err)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("%s is not a directory", path)
	}
	return true, nil
}

func normalizeTargetDir(targetDir string) (string, error) {
	targetDir = strings.TrimLeft(targetDir, "/")
	clean := filepath.Clean(targetDir)

	// Treat / as relative to the container root.
	if clean == "." {
		return "/", nil
	}
	if clean == ".." ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("target directory must not escape the filesystem root")
	}
	return "/" + clean, nil
}

func computeTargetHash(targetDir string) (string, error) {
	normalized, err := normalizeTargetDir(targetDir)
	if err != nil {
		return "", err
	}
	if normalized == "" {
		return "root", nil
	}
	hash := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(hash[:8]), nil
}

func computeTargetTempDir(ckptsDiffDir string, targetDir string) (string, error) {
	targetCacheKey, err := computeTargetHash(targetDir)
	if err != nil {
		return "", err
	}
	temp_id := "temp-" + targetCacheKey
	targetTempDir := filepath.Join(ckptsDiffDir, temp_id)
	return targetTempDir, nil
}

// diffResultVersion is part of a cached result's file name, so results written
// in an older format are not reused. Bump it when the output format changes.
const diffResultVersion = 2

func computeResultPath(ckptsDiffDir string, targetDir string) (string, error) {
	targetCacheKey, err := computeTargetHash(targetDir)
	if err != nil {
		return "", err
	}
	res_id := fmt.Sprintf("result-v%d-%s", diffResultVersion, targetCacheKey)
	targetCacheResult := filepath.Join(ckptsDiffDir, res_id+".json")
	return targetCacheResult, nil
}

func computeCkptsDiffID(leftCkptID, rightCkptID string) string {
	sum := sha256.Sum256([]byte(leftCkptID + "/" + rightCkptID)) // ensures unique pair because ckptIDs can't include /
	hash := hex.EncodeToString(sum[:8])
	return fmt.Sprintf("%.5s-%.5s-%s", leftCkptID, rightCkptID, hash) // add a bit of ckpt names for readability
}

func newDiffFS(m *Manager, overlayDir, leftCkptID, rightCkptID string) (*DiffFS, error) {
	leftMetadata, err := m.loadMetadata(leftCkptID)
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata for checkpoint %s: %w", leftCkptID, err)
	}
	rightMetadata, err := m.loadMetadata(rightCkptID)
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata for checkpoint %s: %w", rightCkptID, err)
	}
	if leftMetadata.Status != CheckpointStatusReady {
		return nil, fmt.Errorf("checkpoint %s is not ready (status: %s)", leftCkptID, leftMetadata.Status)
	}
	if rightMetadata.Status != CheckpointStatusReady {
		return nil, fmt.Errorf("checkpoint %s is not ready (status: %s)", rightCkptID, rightMetadata.Status)
	}
	leftLowerIDs := append([]string(nil), leftMetadata.LayerIDs...)
	rightLowerIDs := append([]string(nil), rightMetadata.LayerIDs...)

	leftOriginalDir := leftMetadata.OriginalDir
	if leftOriginalDir == "" {
		leftOriginalDir = m.originalDir
	}
	rightOriginalDir := rightMetadata.OriginalDir
	if rightOriginalDir == "" {
		rightOriginalDir = m.originalDir
	}
	return &DiffFS{
		overlayDir:       overlayDir,
		leftLayerIDs:     leftLowerIDs,
		rightLayerIDs:    rightLowerIDs,
		leftOriginalDir:  leftOriginalDir,
		rightOriginalDir: rightOriginalDir,
		leftMountPoint:   filepath.Join(overlayDir, diffLeftMount),
		rightMountPoint:  filepath.Join(overlayDir, diffRightMount),
	}, nil
}

const (
	diffLeftMount  = "left_mount"
	diffRightMount = "right_mount"
)

// lockDiffWorkspace opens a workspace dir and takes an exclusive flock on it
// without waiting. busy reports that a running diff holds the lock; the
// returned file is then open but not locked.
func lockDiffWorkspace(dir string) (f *os.File, busy bool, err error) {
	f, err = os.Open(dir)
	if err != nil {
		return nil, false, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return f, true, nil
		}
		f.Close()
		return nil, false, fmt.Errorf("lock %s failed: %w", dir, err)
	}
	return f, false, nil
}

// removeDiffWorkspace unmounts both views and deletes the workspace dir.
func removeDiffWorkspace(dir string) error {
	for _, name := range []string{diffRightMount, diffLeftMount} {
		mountPoint := filepath.Join(dir, name)
		err := unix.Unmount(mountPoint, unix.MNT_DETACH)
		if err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("unmount %s failed: %w", mountPoint, err)
		}
	}
	return os.RemoveAll(dir)
}

// close removes the workspace and releases its lock. Later calls do nothing.
func (d *DiffFS) close() {
	if d.lock == nil {
		return
	}
	_ = removeDiffWorkspace(d.overlayDir)
	_ = d.lock.Close()
	d.lock = nil
}

// writeDiffResult stores a result with a rename and no fsync. It runs under
// the session lock. A file left incomplete by a crash fails the check on read.
func writeDiffResult(resultPath string, data []byte) error {
	tmp := resultPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, resultPath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// claimDiffWorkspace runs under the session lock and returns one of: the cached
// result, a new locked workspace, or the workspace dir of a running diff.
func (m *Manager) claimDiffWorkspace(ckptIDFirst, ckptIDSecond, resultPath, workspaceDir string) (*DiffFS, []byte, *os.File, error) {
	cachedData, err := os.ReadFile(resultPath)
	if err == nil && json.Valid(cachedData) {
		// case 1: already have cached result
		return nil, cachedData, nil, nil
	} else if err != nil && !os.IsNotExist(err) {
		return nil, nil, nil, fmt.Errorf("read cached diff result: %w", err)
	}
	// A result that is not complete JSON is computed again and overwritten.

	lock, busy, err := lockDiffWorkspace(workspaceDir)
	if err == nil {
		if busy {
			// case 2: a diff on this target is running
			return nil, nil, lock, nil
		}
		// case 3: workspace left by an interrupted diff -> remove it, then as case 4
		err := removeDiffWorkspace(workspaceDir)
		lock.Close()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("remove interrupted diff workspace %s: %w", workspaceDir, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, nil, nil, err
	}

	// case 4: no workspace on this target -> create and lock one
	d, err := newDiffFS(m, workspaceDir, ckptIDFirst, ckptIDSecond)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := os.MkdirAll(filepath.Dir(workspaceDir), 0o755); err != nil {
		return nil, nil, nil, fmt.Errorf("mkdir %s failed: %w", filepath.Dir(workspaceDir), err)
	}
	if err := os.Mkdir(workspaceDir, 0o700); err != nil {
		return nil, nil, nil, fmt.Errorf("mkdir %s failed: %w", workspaceDir, err)
	}
	d.lock, busy, err = lockDiffWorkspace(workspaceDir)
	if err == nil && busy {
		d.lock.Close()
		err = fmt.Errorf("new diff workspace %s is locked", workspaceDir)
	}
	if err != nil {
		_ = os.RemoveAll(workspaceDir)
		return nil, nil, nil, err
	}
	for _, dir := range []string{d.leftMountPoint, d.rightMountPoint} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			d.close()
			return nil, nil, nil, fmt.Errorf("mkdir %s failed: %w", dir, err)
		}
	}
	return d, nil, nil, nil
}

func (m *Manager) setupDiffFS(ckptIDFirst, ckptIDSecond, normalizedTargetDir string) (*DiffFS, []byte, error) {
	ckptsDiffID := computeCkptsDiffID(ckptIDFirst, ckptIDSecond)
	ckptsDiffDir := m.diffDir(ckptsDiffID)
	targetCacheResult, err := computeResultPath(ckptsDiffDir, normalizedTargetDir)
	if err != nil {
		return nil, nil, err
	}
	targetTempDir, err := computeTargetTempDir(ckptsDiffDir, normalizedTargetDir)
	if err != nil {
		return nil, nil, err
	}

	var d *DiffFS
	for d == nil {
		var cachedData []byte
		var running *os.File
		if err := m.withSessionLock(func() error {
			var err error
			d, cachedData, running, err = m.claimDiffWorkspace(ckptIDFirst, ckptIDSecond, targetCacheResult, targetTempDir)
			return err
		}); err != nil {
			return nil, nil, err
		}
		if cachedData != nil {
			return nil, cachedData, nil
		}
		if running != nil {
			// Wait for the running diff, then look for its result.
			err := unix.Flock(int(running.Fd()), unix.LOCK_SH)
			running.Close()
			if err != nil {
				return nil, nil, fmt.Errorf("wait for the running diff in %s: %w", targetTempDir, err)
			}
		}
	}

	// Mount
	// layerIDs is ordered oldest to newest, overlayLowerDirs inverts this order to give priority to newest
	leftOverlayLowers := overlayLowerDirs(m.checkpointsDir(), d.leftLayerIDs, d.leftOriginalDir)
	rightOverlayLowers := overlayLowerDirs(m.checkpointsDir(), d.rightLayerIDs, d.rightOriginalDir)

	optionsLeft := fmt.Sprintf("lowerdir=%s", strings.Join(leftOverlayLowers, ":"))
	if err := unix.Mount("overlay", d.leftMountPoint, "overlay", unix.MS_RDONLY, optionsLeft); err != nil {
		d.close()
		return nil, nil, fmt.Errorf("mount overlay at %s failed: %w", d.leftMountPoint, err)
	}

	optionsRight := fmt.Sprintf("lowerdir=%s", strings.Join(rightOverlayLowers, ":"))
	if err := unix.Mount("overlay", d.rightMountPoint, "overlay", unix.MS_RDONLY, optionsRight); err != nil {
		d.close()
		return nil, nil, fmt.Errorf("mount overlay at %s failed: %w", d.rightMountPoint, err)
	}

	return d, nil, nil
}

func processProcMod(leftInfo, rightInfo *ProcInfo, detected *[]DiffProcRecord) error {
	if leftInfo.PID != rightInfo.PID {
		return fmt.Errorf("comparing procs with two different pids")
	}
	var changes []ProcChange
	if leftInfo.PPID != rightInfo.PPID {
		changes = append(changes, ProcChange{
			Field:    "ppid",
			Previous: leftInfo.PPID,
			New:      rightInfo.PPID,
		})
	}
	if leftInfo.PGID != rightInfo.PGID {
		changes = append(changes, ProcChange{
			Field:    "pgid",
			Previous: leftInfo.PGID,
			New:      rightInfo.PGID,
		})
	}
	if leftInfo.SID != rightInfo.SID {
		changes = append(changes, ProcChange{
			Field:    "sid",
			Previous: leftInfo.SID,
			New:      rightInfo.SID,
		})
	}
	if leftInfo.Comm != rightInfo.Comm {
		changes = append(changes, ProcChange{
			Field:    "comm",
			Previous: leftInfo.Comm,
			New:      rightInfo.Comm,
		})
	}
	if len(changes) > 0 {
		*detected = append(*detected, DiffProcRecord{
			PID:        leftInfo.PID,
			ChangeType: ChangeTypeModified,
			Changes:    changes,
		})
	}
	return nil
}

// modification of crit's ExplorePs()
func explorePsFlat(inputDirPath string) (map[uint32]ProcInfo, error) {
	processes := make(map[uint32]ProcInfo)

	psTreeFile, err := os.Open(filepath.Join(inputDirPath, "pstree.img"))
	if err != nil {
		return nil, err
	}
	psTreeCrit := crit.New(psTreeFile, nil, "", false, false)
	psTreeImg, err := psTreeCrit.Decode(&pstree.PstreeEntry{})
	psTreeFile.Close()
	if err != nil {
		return nil, err
	}

	for _, entry := range psTreeImg.Entries {
		process, ok := entry.Message.(*pstree.PstreeEntry)
		if !ok {
			return nil, fmt.Errorf("unexpected pstree entry type")
		}
		pid := process.GetPid()

		coreFile, err := os.Open(filepath.Join(inputDirPath, fmt.Sprintf("core-%d.img", pid)))
		if err != nil {
			return nil, err
		}
		coreCrit := crit.New(coreFile, nil, "", false, false)
		coreImg, err := coreCrit.Decode(&criu_core.CoreEntry{})
		coreFile.Close()
		if err != nil {
			return nil, err
		}
		if len(coreImg.Entries) == 0 {
			return nil, fmt.Errorf("no core entries for PID %d", pid)
		}
		coreData, ok := coreImg.Entries[0].Message.(*criu_core.CoreEntry)
		if !ok {
			return nil, fmt.Errorf("unexpected core entry type for PID %d", pid)
		}

		processes[pid] = ProcInfo{
			PID:  pid,
			PPID: process.GetPpid(),
			PGID: process.GetPgid(),
			SID:  process.GetSid(),
			Comm: coreData.GetTc().GetComm(),
		}
	}
	return processes, nil
}

func (m *Manager) getProcDiff(ckptIDLeft, ckptIDRight string, detected *[]DiffProcRecord) error {
	// NOTE: treating pid as identity, so pid reuse will show up as modified
	// so TODO: strengthen identity key

	// to get process diff, need to get the criu directories for both checkpoints
	criuDirLeft := m.checkpointCriuDir(ckptIDLeft)
	criuDirRight := m.checkpointCriuDir(ckptIDRight)
	// Hold the images lock shared, as a restore does, so a tmpfs flush
	// cannot repoint the images while they are read.
	if unlock, err := lockImages(m.checkpointDir(ckptIDLeft), false); err == nil {
		defer unlock()
	}
	if unlock, err := lockImages(m.checkpointDir(ckptIDRight), false); err == nil {
		defer unlock()
	}
	expLeft, err := explorePsFlat(criuDirLeft)
	if err != nil {
		return err
	}
	expRight, err := explorePsFlat(criuDirRight)
	if err != nil {
		return err
	}

	for pidLeft, infoLeft := range expLeft {
		infoRight, exists := expRight[pidLeft]
		if !exists {
			*detected = append(*detected, DiffProcRecord{
				PID:        pidLeft,
				ChangeType: ChangeTypeDeleted,
				Process:    &infoLeft,
			})
			continue
		}
		if err := processProcMod(&infoLeft, &infoRight, detected); err != nil {
			return err
		}
	}
	for pid, info := range expRight {
		if _, exists := expLeft[pid]; exists {
			continue
		}
		*detected = append(*detected, DiffProcRecord{
			PID:        pid,
			ChangeType: ChangeTypeAdded,
			Process:    &info,
		})
	}
	return nil
}

// getFSDiff compares the two mounted views below normalizedTargetDir.
func (m *Manager) getFSDiff(d *DiffFS, normalizedTargetDir string) ([]DiffFileRecord, error) {
	// TODO: openroot will report error on absolute symlinks (doesn't resolve as relative to container root)
	leftRoot, err := os.OpenRoot(d.leftMountPoint)
	if err != nil {
		return nil, err
	}
	defer leftRoot.Close()
	rightRoot, err := os.OpenRoot(d.rightMountPoint)
	if err != nil {
		return nil, err
	}
	defer rightRoot.Close()
	view := fsView{
		leftRoot:               leftRoot,
		rightRoot:              rightRoot,
		containerViewTargetDir: normalizedTargetDir,
	}

	leftExists, err := existsTargetDir(&view, LeftSide)
	if err != nil {
		return nil, err
	}
	rightExists, err := existsTargetDir(&view, RightSide)
	if err != nil {
		return nil, err
	}

	detectedFS := make([]DiffFileRecord, 0)
	switch {
	case !leftExists && !rightExists:
		// target dne so no diff
	case leftExists && !rightExists:
		if err := recurseSideChangeType(&view, LeftSide, normalizedTargetDir, fs.ModeDir, &detectedFS, ChangeTypeDeleted); err != nil {
			return nil, err
		}
	case !leftExists && rightExists:
		if err := recurseSideChangeType(&view, RightSide, normalizedTargetDir, fs.ModeDir, &detectedFS, ChangeTypeAdded); err != nil {
			return nil, err
		}
	default:
		if err := m.getAllCandidates(d, &view); err != nil {
			return nil, err
		}
		if err := walkFS(&view, normalizedTargetDir, &detectedFS); err != nil {
			return nil, err
		}
	}
	return detectedFS, nil
}

// Entry point for diff
func (m *Manager) PrintDiff(ckptIDFirst, ckptIDSecond, targetDir string) error {
	normalizedTargetDir, err := normalizeTargetDir(targetDir)
	if err != nil {
		return err
	}
	if err := validateCheckpointID(ckptIDFirst); err != nil {
		return fmt.Errorf("invalid checkpoint ID %s: %w", ckptIDFirst, err)
	}
	if err := validateCheckpointID(ckptIDSecond); err != nil {
		return fmt.Errorf("invalid checkpoint ID %s: %w", ckptIDSecond, err)
	}
	if ckptIDFirst == ckptIDSecond {
		metadata, err := m.loadMetadata(ckptIDFirst)
		if err != nil {
			return fmt.Errorf("failed to read metadata for checkpoint %s: %w", ckptIDFirst, err)
		}
		if metadata.Status != CheckpointStatusReady {
			return fmt.Errorf("checkpoint %s is not ready (status: %s)", ckptIDFirst, metadata.Status)
		}
		result := DiffResult{
			From: ckptIDFirst,
			To:   ckptIDSecond,
			Filesystem: FilesystemDiff{
				TargetDir: normalizedTargetDir,
				Records:   []DiffFileRecord{},
			},
			Processes: []DiffProcRecord{},
		}
		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal diff result: %w", err)
		}
		if _, err := os.Stdout.Write(append(data, '\n')); err != nil {
			return fmt.Errorf("write diff result to stdout: %w", err)
		}
		return nil
	}

	// FS
	d, cachedData, err := m.setupDiffFS(ckptIDFirst, ckptIDSecond, normalizedTargetDir)
	if err != nil {
		return err
	}
	if cachedData != nil {
		if _, err := os.Stdout.Write(append(cachedData, '\n')); err != nil {
			return fmt.Errorf("write cached diff result to stdout: %w", err)
		}
		return nil
	}

	defer d.close()

	// The filesystem and process diffs share nothing and run concurrently.
	detectedProc := make([]DiffProcRecord, 0)
	procDone := make(chan error, 1)
	go func() {
		procDone <- m.getProcDiff(ckptIDFirst, ckptIDSecond, &detectedProc)
	}()
	detectedFS, fsErr := m.getFSDiff(d, normalizedTargetDir)
	procErr := <-procDone
	if fsErr != nil {
		return fsErr
	}
	if procErr != nil {
		return procErr
	}
	sort.Slice(detectedFS, func(i, j int) bool {
		return detectedFS[i].Path < detectedFS[j].Path
	})
	sort.Slice(detectedProc, func(i, j int) bool {
		return detectedProc[i].PID < detectedProc[j].PID
	})

	// Result all
	ckptsDiffID := computeCkptsDiffID(ckptIDFirst, ckptIDSecond)
	ckptsDiffDir := m.diffDir(ckptsDiffID)
	resultPath, err := computeResultPath(ckptsDiffDir, normalizedTargetDir)
	if err != nil {
		return err
	}
	result := DiffResult{
		From: ckptIDFirst,
		To:   ckptIDSecond,
		Filesystem: FilesystemDiff{
			TargetDir: normalizedTargetDir,
			Records:   detectedFS,
		},
		Processes: detectedProc,
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal diff result: %w", err)
	}

	if err := m.withSessionLock(func() error {
		return writeDiffResult(resultPath, data)
	}); err != nil {
		return err
	}
	// Diffs waiting on this workspace go on once it is closed, before stdout is read.
	d.close()
	if _, err := os.Stdout.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write diff result to stdout: %w", err)
	}

	return nil
}
