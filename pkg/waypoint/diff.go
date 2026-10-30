package waypoint

// TODO add description

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sort"
	"syscall"
	"crypto/sha256"
    "encoding/hex"

	"golang.org/x/sys/unix"
)

type FileType string
// types currently tracked
const (
	FileTypeSymlink		FileType = "symlink"
	FileTypeRegular 	FileType = "regular"
	FileTypeDevice		FileType = "device"
	FileTypeFIFO		FileType = "fifo" // currently only add/delete tracked
	FileTypeSocket		FileType = "socket" // currently only add/delete tracked
	FileTypeOther		FileType = "other"
	FileTypeModified	FileType = "modified" // TODO: use for files whose mode changed, OK?
)

type ChangeType string
const (
	ChangeTypeAdded		ChangeType = "added"
	ChangeTypeDeleted	ChangeType = "deleted"
	ChangeTypeModified  ChangeType = "modified"
)

type DiffFileRecord struct {
	Path            string           `json:"path"`
	ChangeType		ChangeType       `json:"change_type"`
	FileType        FileType         `json:"file_type"`
}

type DiffResult		struct {
	TargetDir	string 				`json:"target_dir"`
	Records		[]DiffFileRecord	`json:"records,omitempty"`
}

type DiffFS struct {
	overlayDir             string

	leftOriginalDir		string
	rightOriginalDir	string

	leftLayerIDs    	[]string 
	rightLayerIDs   	[]string 

	leftUpperDir    	string   
	rightUpperDir   	string   
	leftWorkDir     	string   
	rightWorkDir    	string

	leftMountPoint  	string
	rightMountPoint		string
}

func cmpIsDiff(leftFilePath, rightFilePath string) (bool, error) {
	cmd := exec.Command("cmp", "-s", leftFilePath, rightFilePath)
	err := cmd.Run()
	if err == nil {
		return false, nil // files are identical
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if exitErr.ExitCode() == 1 {
			return true, nil // files differ
		} else {
			return false, fmt.Errorf("cmp -s %s %s: %w", leftFilePath, rightFilePath, err)
		}
	}
	return false, fmt.Errorf("execute cmp -s %s %s: %w", leftFilePath, rightFilePath, err)
}

func getFileType(path string) (FileType, error) {
	info, err := os.Lstat(path)
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

func processNonDirMod(leftFilePath, rightFilePath, containerViewPath string, detected *[]DiffFileRecord) error {
	leftInfo, err := os.Lstat(leftFilePath)
	if err != nil {
		return fmt.Errorf("lstat %s: %w", leftFilePath, err)
	}
	rightInfo, err := os.Lstat(rightFilePath)
	if err != nil {
		return fmt.Errorf("lstat %s: %w", rightFilePath, err)
	}
	leftFileType, err := getFileType(leftFilePath)
	if err != nil {
		return err
	}
	rightFileType, err := getFileType(rightFilePath)
	if err != nil {
		return err
	}
	// Mode change
	if leftInfo.Mode() != rightInfo.Mode() {
		if (leftFileType == rightFileType) {
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
		res, err := cmpIsDiff(leftFilePath, rightFilePath)
		if err != nil {
			return err
		}
		mod = res
	} else if leftFileType == FileTypeSymlink {
		leftTarget, err := os.Readlink(leftFilePath)
		if err != nil {
			return fmt.Errorf("readlink %s: %w", leftFilePath, err)
		}
		rightTarget, err := os.Readlink(rightFilePath)
		if err != nil {
			return fmt.Errorf("readlink %s: %w", rightFilePath, err)
		}
		mod = (leftTarget != rightTarget)
	} else if leftFileType == FileTypeDevice {
		leftSy, ok := leftInfo.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("unexpected stat type for %s", leftFilePath)
		}
		rightSys, ok := rightInfo.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("unexpected stat type for %s", rightFilePath)
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

func recurseWithChangeType(rootViewPath, containerViewPath string, detected *[]DiffFileRecord, changeType ChangeType) error {
	info, err := os.Lstat(rootViewPath)
	if err != nil {
		return fmt.Errorf("lstat %s: %w", rootViewPath, err)
	}
	if !info.IsDir() {
		fileType, err := getFileType(rootViewPath)
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
	dentries, err := os.ReadDir(rootViewPath)
	if err != nil {
		return fmt.Errorf("read directory %s: %w", rootViewPath, err)
	}
	for _, dentry := range dentries {
		rootViewSubpath := filepath.Join(rootViewPath, dentry.Name())
		containerViewSubpath := filepath.Join(containerViewPath, dentry.Name())
		if err := recurseWithChangeType(rootViewSubpath, containerViewSubpath, detected, changeType); err != nil {
			return err
		}
	}
	return nil
}

func walkFS(leftDir, rightDir, containerViewDir string, detected *[]DiffFileRecord) error {
    // note: won't follow symlink dirs
	
	leftEntries, err := os.ReadDir(leftDir)
    if err != nil {
        return fmt.Errorf("read directory %s: %w", leftDir, err)
    }
    rightEntries, err := os.ReadDir(rightDir)
    if err != nil {
        return fmt.Errorf("read directory %s: %w", rightDir, err)
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
		rightDentry, exists := rightNameMap[name]
		if !exists {
			// Left-only entry -> deleted
			rootViewSubpath := filepath.Join(leftDir, name)
			if err := recurseWithChangeType(rootViewSubpath, containerViewSubpath, detected, ChangeTypeDeleted); err != nil {
				return err
			}
			continue
		}
		// Left and right entry 
		leftRootViewSubpath := filepath.Join(leftDir, name)
		rightRootViewSubpath := filepath.Join(rightDir, name)
		// both directories, so recurse
		if leftDentry.IsDir() && rightDentry.IsDir() {
			if err := walkFS(leftRootViewSubpath, rightRootViewSubpath, containerViewSubpath, detected); err != nil {
				return err
			}
			continue
		}
		// Just one side is directory -> left is deleted, right is added
		if (leftDentry.IsDir() && !rightDentry.IsDir()) || (!leftDentry.IsDir() && rightDentry.IsDir()){
			if err := recurseWithChangeType(leftRootViewSubpath, containerViewSubpath, detected, ChangeTypeDeleted); err != nil {
				return err
			}
			if err := recurseWithChangeType(rightRootViewSubPath, containerViewSubpath, detected, ChangeTypeAdded); err != nil {
				return err
			}
			continue
		}
		// Neither are directories -> check for modification
		if err := processNonDirMod(leftRootViewSubpath, rightRootViewSubpath, containerViewSubpath, detected); err != nil {
			return err
		}
	}
	// Right-only entries -> added
	for name, dentry := range rightNameMap {
		if _, exists := leftNameMap[name]; exists {
			continue
		}
		rootViewSubpath := filepath.Join(rightDir, name)
		if err := recurseWithChangeType(rootViewSubpath, containerViewSubpath, detected, ChangeTypeAdded); err != nil {
			return err
		}
		continue
	}
    return nil
}

func existsDir(path string) (bool, error) {
    info, err := os.Lstat(path) // requires non symlink for the target dir running diff on. OK?
    if err != nil {
        if os.IsNotExist(err) {
            return false, nil
        }
        return false, fmt.Errorf("stat %s: %w", path, err)
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
		overlayDir:         overlayDir,
		leftLayerIDs:       leftLowerIDs,
		rightLayerIDs:      rightLowerIDs,
		leftOriginalDir:	leftOriginalDir,
		rightOriginalDir:	rightOriginalDir,
		leftUpperDir:       filepath.Join(overlayDir, "left_upper"),
		rightUpperDir:      filepath.Join(overlayDir, "right_upper"),
		leftWorkDir:        filepath.Join(overlayDir, "left_work"),
		rightWorkDir:       filepath.Join(overlayDir, "right_work"),
		leftMountPoint:     filepath.Join(overlayDir, "left_mount"),
		rightMountPoint:    filepath.Join(overlayDir, "right_mount"),
	}, nil
}

func normalizeTargetDir(targetDir string) (string, error) {
    targetDir = strings.TrimLeft(targetDir, "/")
	clean := filepath.Clean(targetDir)

	// Treat / as relative to the container root.
	if clean == "." {
        return "", nil
    }
    if clean == ".." ||
        strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
        return "", fmt.Errorf("target directory must not escape the filesystem root")
    }
    return clean, nil
}

func computeTargetHash(targetDir string) (string, error) {
	err, normalized := normalizeTargetDir(targetDir)
	if err != nil {
		return "", err
	}
	if normalized == "" {
		return "root", nil
	}
	hash := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(hash[:]), nil
}

func computeTargetTempDir(ckptsDiffDir string, targetDir string) (string, error) {
	err, targetCacheKey := computeTargetHash(targetDir)
	if err != nil {
		return err, nil
	}
	temp_id := "temp-" + targetCacheKey
	targetTempDir := filepath.Join(ckptsDiffDir, temp_id)
	return targetTempDir, nil
}

func computeResultPath(ckptsDiffDir string, targetDir string) (string, error) {
	err, targetCacheKey := computeTargetHash(targetDir)
	if err != nil {
		return err, nil
	}
	res_id := "result-" + targetCacheKey 
	targetCacheResult := filepath.Join(ckptsDiffDir, res_id+".json")
	return targetCacheResult, nil
}

func computeCkptsDiffID(leftCkptID, rightCkptID string) string {
	return fmt.Sprintf("%s-%s", leftCkptID, rightCkptID) // TODO make more robust, maybe hash as well
}

func (m *Manager) setupDiffFS(ckptIDFirst, ckptIDSecond, normalizedTargetDir string) (*DiffFS, bool, error) {
	if err := validateCheckpointID(ckptIDFirst); err != nil {
		return nil, false, fmt.Errorf("invalid checkpoint ID %s: %w", ckptIDFirst, err)
	}
	if err := validateCheckpointID(ckptIDSecond); err != nil {
		return nil, false, fmt.Errorf("invalid checkpoint ID %s: %w", ckptIDSecond, err)
	}

	ckptsDiffID := computeCkptsDiffID(ckptIDFirst, ckptIDSecond) 
	ckptsDiffDir := m.diffDir(ckptsDiffID)
    err, targetCacheResult := computeResultPath(ckptsDiffDir, normalizedTargetDir)
	if err != nil {
		return nil, false, err
	}
	err, targetTempDir := computeTargetTempDir(ckptsDiffDir, normalizedTargetDir)
	if err != nil {
		return nil, false, err
	}

	var d *DiffFS
	cacheExists := false
	if err := m.withSessionLock(func() error {
		_, errCkpt := os.Stat(ckptsDiffDir)
		_, errTemp := os.Stat(targetTempDir)
		_, errRes := os.Stat(targetCacheResult)
		if errRes == nil  {
			// case 1: already have cached result
			cacheExists = true
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
		for _, dir := range []string{d.leftMountPoint, d.rightMountPoint, d.leftUpperDir, d.rightUpperDir, d.leftWorkDir, d.rightWorkDir} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("mkdir %s failed: %w", dir, err)
			}
		}
		return nil
	}); err != nil {
		return nil, false, err
	}
	if cacheExists {
		return nil, true, nil
	}

	// Mount
	// layerIDs is ordered oldest to newest, overlayLowerDirs inverts this order to give priority to newest
	leftOverlayLowers := overlayLowerDirs(m.checkpointsDir(), d.leftLayerIDs, d.leftOriginalDir)
	rightOverlayLowers := overlayLowerDirs(m.checkpointsDir(), d.rightLayerIDs, d.rightOriginalDir)

	optionsLeft := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
		strings.Join(leftOverlayLowers, ":"), d.leftUpperDir, d.leftWorkDir)
	if err := unix.Mount("overlay", d.leftMountPoint, "overlay", 0, optionsLeft); err != nil {
		_ = os.RemoveAll(d.overlayDir)
		return nil, false, fmt.Errorf("mount overlay at %s failed: %w", d.leftMountPoint, err)
	}

	optionsRight := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
		strings.Join(rightOverlayLowers, ":"), d.rightUpperDir, d.rightWorkDir)
	if err := unix.Mount("overlay", d.rightMountPoint, "overlay", 0, optionsRight); err != nil {
		_ = unix.Unmount(d.leftMountPoint, unix.MNT_DETACH)
		_ = os.RemoveAll(d.overlayDir)
		return nil, false, fmt.Errorf("mount overlay at %s failed: %w", d.rightMountPoint, err)
	}

	return d, false, nil
}

// Entry point for diff
func (m *Manager) PrintDiff(ckptIDFirst, ckptIDSecond, targetDir string) (error) {
	normalizedTargetDir, err := normalizeTargetDir(targetDir)
	if err != nil {
		return err
	}
	
	d, cached, err := m.setupDiffFS(ckptIDFirst, ckptIDSecond, normalizedTargetDir)
	if err != nil {
		return err
	}
	if cached {
		// DONE, ask on slack about return 
		return nil
	}

	defer func() {
    _ = unix.Unmount(d.rightMountPoint, unix.MNT_DETACH)
    _ = unix.Unmount(d.leftMountPoint, unix.MNT_DETACH)
    _ = os.RemoveAll(d.overlayDir)
	}()

	leftTargetDir := filepath.Join(d.leftMountPoint, normalizedTargetDir)
	rightTargetDir := filepath.Join(d.rightMountPoint, normalizedTargetDir)
	
	leftExists, err := existsDir(leftTargetDir)
	if err != nil {
		return err
	}
	rightExists, err := existsDir(rightTargetDir)
	if err != nil {
		return err
	}
	detected := make([]DiffFileRecord, 0)
	switch {
	case !leftExists && !rightExists:
	case leftExists && !rightExists:
		if err := recurseWithChangeType(leftTargetDir, normalizedTargetDir, &detected, ChangeTypeDeleted); err != nil {
			return err
		}
	case !leftExists && rightExists:
		if err := recurseWithChangeType(rightTargetDir, normalizedTargetDir, &detected, ChangeTypeAdded); err != nil {
			return err
		}
	default:
		if err := walkFS(leftTargetDir, rightTargetDir, normalizedTargetDir, &detected); err != nil {
			return err
		}
	}

	sort.Slice(detected, func(i, j int) bool {
		return detected[i].Path < detected[j].Path
	})

	ckptsDiffID := computeCkptsDiffID(ckptIDFirst, ckptIDSecond) 
	ckptsDiffDir := m.diffDir(ckptsDiffID)
    err, resultPath := computeResultPath(ckptsDiffDir, normalizedTargetDir)
	if err != nil {
		return err
	}
	
	result := DiffResult{
		TargetDir: normalizedTargetDir,
		Files: detected,
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

	return nil
}