package waypoint

// TODO add description

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
}

type FileType string

// types currently tracked
const (
	FileTypeSymlink  FileType = "symlink"
	FileTypeRegular  FileType = "regular"
	FileTypeDevice   FileType = "device" // modified = major/minor number change
	FileTypeFIFO     FileType = "fifo"   // currently only add/delete tracked
	FileTypeSocket   FileType = "socket" // currently only add/delete tracked
	FileTypeOther    FileType = "other"
	FileTypeModified FileType = "modified" // TODO: use for files whose mode changed, OK?
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

	leftOriginalDir  string
	rightOriginalDir string

	leftLayerIDs  []string
	rightLayerIDs []string

	leftMountPoint  string
	rightMountPoint string
}

func areRegFilesDiff(leftRoot, rightRoot *os.Root, path string) (bool, error) {
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
	const chunkSize = 32 * 1024
	leftBuf := make([]byte, chunkSize)
	rightBuf := make([]byte, chunkSize)

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

func getFileType(root *os.Root, path string) (FileType, error) {
	info, err := root.Lstat(path)
	if err != nil {
		return FileTypeOther, fmt.Errorf("lstat %s: %w", path, err)
	}
	mode := info.Mode()
	if mode.IsRegular() {
		return FileTypeRegular, nil
	}
	if mode&os.ModeSymlink != 0 {
		return FileTypeSymlink, nil
	}
	if mode&os.ModeDevice != 0 {
		return FileTypeDevice, nil
	}
	if mode&os.ModeNamedPipe != 0 {
		return FileTypeFIFO, nil
	}
	if mode&os.ModeSocket != 0 {
		return FileTypeSocket, nil
	}
	return FileTypeOther, nil
}

func processNonDirMod(leftRoot, rightRoot *os.Root, rootViewPath, containerViewPath string, detected *[]DiffFileRecord) error {
	leftInfo, err := leftRoot.Lstat(rootViewPath)
	if err != nil {
		return fmt.Errorf("lstat %s: %w", rootViewPath, err)
	}
	rightInfo, err := rightRoot.Lstat(rootViewPath)
	if err != nil {
		return fmt.Errorf("lstat %s: %w", rootViewPath, err)
	}
	leftFileType, err := getFileType(leftRoot, rootViewPath)
	if err != nil {
		return err
	}
	rightFileType, err := getFileType(rightRoot, rootViewPath)
	if err != nil {
		return err
	}
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
				Path:       containerViewPath,
				ChangeType: ChangeTypeModified,
				FileType:   FileTypeModified,
			})
		}
		return nil
	}
	// Other changes currently tracked: cmp on regular, symlink target, dev major/minor. TODO support other changes/types
	mod := false
	if leftFileType == FileTypeRegular {
		res, err := areRegFilesDiff(leftRoot, rightRoot, rootViewPath)
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

func recurseWithChangeType(root *os.Root, rootViewPath, containerViewPath string, detected *[]DiffFileRecord, changeType ChangeType) error {
	info, err := root.Lstat(rootViewPath)
	if err != nil {
		return fmt.Errorf("lstat %s: %w", rootViewPath, err)
	}
	if !info.IsDir() {
		fileType, err := getFileType(root, rootViewPath)
		if err != nil {
			return err
		}
		*detected = append(*detected, DiffFileRecord{
			Path:       containerViewPath,
			ChangeType: changeType,
			FileType:   fileType,
		})
		return nil
	}

	dir, err := root.Open(rootViewPath)
	if err != nil {
		return fmt.Errorf("open directory %s: %w", rootViewPath, err)
	}
	dentries, readErr := dir.ReadDir(-1)
	closeErr := dir.Close()
	if readErr != nil {
		return fmt.Errorf("read directory %s: %w", rootViewPath, readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close directory %s: %w", rootViewPath, closeErr)
	}
	for _, dentry := range dentries {
		rootViewSubpath := filepath.Join(rootViewPath, dentry.Name())
		containerViewSubpath := filepath.Join(containerViewPath, dentry.Name())
		if err := recurseWithChangeType(root, rootViewSubpath, containerViewSubpath, detected, changeType); err != nil {
			return err
		}
	}
	return nil
}

func walkFS(leftRoot, rightRoot *os.Root, rootViewDir, containerViewDir string, detected *[]DiffFileRecord) error {
	// note: won't follow symlink dirs
	// TODO make this faster
	leftDir, err := leftRoot.Open(rootViewDir)
	if err != nil {
		return fmt.Errorf("open left directory %s: %w", rootViewDir, err)
	}
	leftEntries, readErr := leftDir.ReadDir(-1)
	closeErr := leftDir.Close()
	if readErr != nil {
		return fmt.Errorf("read left directory %s: %w", rootViewDir, readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close left directory %s: %w", rootViewDir, closeErr)
	}

	rightDir, err := rightRoot.Open(rootViewDir)
	if err != nil {
		return fmt.Errorf("open right directory %s: %w", rootViewDir, err)
	}
	rightEntries, readErr := rightDir.ReadDir(-1)
	closeErr = rightDir.Close()
	if readErr != nil {
		return fmt.Errorf("read right directory %s: %w", rootViewDir, readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close right directory %s: %w", rootViewDir, closeErr)
	}

	leftNameMap := make(map[string]os.DirEntry, len(leftEntries))
	for _, entry := range leftEntries {
		leftNameMap[entry.Name()] = entry
	}
	rightNameMap := make(map[string]os.DirEntry, len(rightEntries))
	for _, entry := range rightEntries {
		rightNameMap[entry.Name()] = entry
	}

	for name, leftDentry := range leftNameMap {
		containerViewSubpath := filepath.Join(containerViewDir, name)
		rootViewSubpath := filepath.Join(rootViewDir, name)
		rightDentry, exists := rightNameMap[name]
		if !exists {
			// Left-only entry -> deleted
			if err := recurseWithChangeType(leftRoot, rootViewSubpath, containerViewSubpath, detected, ChangeTypeDeleted); err != nil {
				return err
			}
			continue
		}
		// Left and right entry
		// both directories, so recurse
		if leftDentry.IsDir() && rightDentry.IsDir() {
			if err := walkFS(leftRoot, rightRoot, rootViewSubpath, containerViewSubpath, detected); err != nil {
				return err
			}
			continue
		}
		// Just one side is directory -> left is deleted, right is added
		if (leftDentry.IsDir() && !rightDentry.IsDir()) || (!leftDentry.IsDir() && rightDentry.IsDir()) {
			if err := recurseWithChangeType(leftRoot, rootViewSubpath, containerViewSubpath, detected, ChangeTypeDeleted); err != nil {
				return err
			}
			if err := recurseWithChangeType(rightRoot, rootViewSubpath, containerViewSubpath, detected, ChangeTypeAdded); err != nil {
				return err
			}
			continue
		}
		// Neither are directories -> check for modification
		if err := processNonDirMod(leftRoot, rightRoot, rootViewSubpath, containerViewSubpath, detected); err != nil {
			return err
		}
	}
	// Right-only entries -> added
	for name := range rightNameMap {
		if _, exists := leftNameMap[name]; exists {
			continue
		}
		containerViewSubpath := filepath.Join(containerViewDir, name)
		rootViewSubpath := filepath.Join(rootViewDir, name)
		if err := recurseWithChangeType(rightRoot, rootViewSubpath, containerViewSubpath, detected, ChangeTypeAdded); err != nil {
			return err
		}
		continue
	}
	return nil
}

func existsTargetDir(root *os.Root, path string) (bool, error) {
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
		leftMountPoint:   filepath.Join(overlayDir, "left_mount"),
		rightMountPoint:  filepath.Join(overlayDir, "right_mount"),
	}, nil
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

func computeResultPath(ckptsDiffDir string, targetDir string) (string, error) {
	targetCacheKey, err := computeTargetHash(targetDir)
	if err != nil {
		return "", err
	}
	res_id := "result-" + targetCacheKey
	targetCacheResult := filepath.Join(ckptsDiffDir, res_id+".json")
	return targetCacheResult, nil
}

func computeCkptsDiffID(leftCkptID, rightCkptID string) string {
	sum := sha256.Sum256([]byte(leftCkptID + "/" + rightCkptID)) // ensures unique pair because ckptIDs can't include /
	hash := hex.EncodeToString(sum[:8])
	return fmt.Sprintf("%.5s-%.5s-%s", leftCkptID, rightCkptID, hash) // add a bit of ckpt names for readability
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

	var cachedData []byte
	var d *DiffFS
	if err := m.withSessionLock(func() error {
		_, errCkpt := os.Stat(ckptsDiffDir)
		_, errTemp := os.Stat(targetTempDir)
		_, errRes := os.Stat(targetCacheResult)
		if errRes == nil {
			// case 1: already have cached result
			// TODO further validation of cache before try to read?
			var err error
			cachedData, err = os.ReadFile(targetCacheResult)
			if err != nil {
				return fmt.Errorf("read cached diff result: %w", err)
			}
			return nil
		} else if !os.IsNotExist(errRes) {
			return errRes
		}
		if errTemp == nil {
			// case 2: have temp dir for this target but no result yet -> computation already in progress, fail
			return fmt.Errorf("diff workspace %s on target %s already exists but results json not found", targetTempDir, normalizedTargetDir)
		} else if !os.IsNotExist(errTemp) {
			return errTemp
		}
		if errCkpt != nil {
			if os.IsNotExist(errCkpt) {
				// case 3: don't have any diffs on these ckpts yet
				if err := os.MkdirAll(ckptsDiffDir, 0o755); err != nil {
					return fmt.Errorf("mkdir %s failed: %w", ckptsDiffDir, err)
				}
			} else {
				return errCkpt
			}
		}
		newD, err := newDiffFS(m, targetTempDir, ckptIDFirst, ckptIDSecond)
		if err != nil {
			return err
		}
		d = newD
		// case 3,4: have ckpt diff dir, but not yet on this target
		if err := os.Mkdir(targetTempDir, 0o700); err != nil {
			return fmt.Errorf("mkdir %s failed: %w", targetTempDir, err)
		}
		for _, dir := range []string{d.leftMountPoint, d.rightMountPoint} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				_ = os.RemoveAll(targetTempDir)
				return fmt.Errorf("mkdir %s failed: %w", dir, err)
			}
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	if cachedData != nil {
		return nil, cachedData, nil
	}

	// Mount
	// layerIDs is ordered oldest to newest, overlayLowerDirs inverts this order to give priority to newest
	leftOverlayLowers := overlayLowerDirs(m.checkpointsDir(), d.leftLayerIDs, d.leftOriginalDir)
	rightOverlayLowers := overlayLowerDirs(m.checkpointsDir(), d.rightLayerIDs, d.rightOriginalDir)

	optionsLeft := fmt.Sprintf("lowerdir=%s", strings.Join(leftOverlayLowers, ":"))
	if err := unix.Mount("overlay", d.leftMountPoint, "overlay", unix.MS_RDONLY, optionsLeft); err != nil {
		_ = os.RemoveAll(d.overlayDir)
		return nil, nil, fmt.Errorf("mount overlay at %s failed: %w", d.leftMountPoint, err)
	}

	optionsRight := fmt.Sprintf("lowerdir=%s", strings.Join(rightOverlayLowers, ":"))
	if err := unix.Mount("overlay", d.rightMountPoint, "overlay", unix.MS_RDONLY, optionsRight); err != nil {
		_ = unix.Unmount(d.leftMountPoint, unix.MNT_DETACH)
		_ = os.RemoveAll(d.overlayDir)
		return nil, nil, fmt.Errorf("mount overlay at %s failed: %w", d.rightMountPoint, err)
	}

	return d, nil, nil
}

func processProcMod(leftInfo, rightInfo *ProcInfo, detected *[]DiffProcRecord) error {
	// confirm these are the same pid, else shoudln't be comparing
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

	defer func() {
		_ = unix.Unmount(d.rightMountPoint, unix.MNT_DETACH)
		_ = unix.Unmount(d.leftMountPoint, unix.MNT_DETACH)
		_ = os.RemoveAll(d.overlayDir)
	}()

	// TODO: openroot will report error on absolute symlinks (doesn't resolve as relative to container root)
	leftRoot, err := os.OpenRoot(d.leftMountPoint)
	if err != nil {
		return err
	}
	defer leftRoot.Close()
	rightRoot, err := os.OpenRoot(d.rightMountPoint)
	if err != nil {
		return err
	}
	defer rightRoot.Close()
	targetForRoot, err := cleanRootPath(normalizedTargetDir)
	if err != nil {
		return err
	}

	leftExists, err := existsTargetDir(leftRoot, targetForRoot)
	if err != nil {
		return err
	}
	rightExists, err := existsTargetDir(rightRoot, targetForRoot)
	if err != nil {
		return err
	}
	detectedFS := make([]DiffFileRecord, 0)
	switch {
	case !leftExists && !rightExists:
	case leftExists && !rightExists:
		if err := recurseWithChangeType(leftRoot, targetForRoot, normalizedTargetDir, &detectedFS, ChangeTypeDeleted); err != nil {
			return err
		}
	case !leftExists && rightExists:
		if err := recurseWithChangeType(rightRoot, targetForRoot, normalizedTargetDir, &detectedFS, ChangeTypeAdded); err != nil {
			return err
		}
	default:
		if err := walkFS(leftRoot, rightRoot, targetForRoot, normalizedTargetDir, &detectedFS); err != nil {
			return err
		}
	}

	sort.Slice(detectedFS, func(i, j int) bool {
		return detectedFS[i].Path < detectedFS[j].Path
	})

	detectedProc := make([]DiffProcRecord, 0)
	if err := m.getProcDiff(ckptIDFirst, ckptIDSecond, &detectedProc); err != nil {
		return err
	}
	sort.Slice(detectedProc, func(i, j int) bool {
		return detectedProc[i].PID < detectedProc[j].PID
	})

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
		return atomicWriteFile(resultPath, data, 0o644)
	}); err != nil {
		return err
	}
	if _, err := os.Stdout.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write diff result to stdout: %w", err)
	}

	return nil
}
