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

	"golang.org/x/sys/unix"
)

type Diff struct {
	rootDir             string

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

	resultsFile	 		string
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

func areNonDirsDiff(leftFilePath, rightFilePath string) (bool, error) {
	leftInfo, err := os.Lstat(leftFilePath)
	if err != nil {
		return false, fmt.Errorf("lstat %s: %w", leftFilePath, err)
	}
	rightInfo, err := os.Lstat(rightFilePath)
	if err != nil {
		return false, fmt.Errorf("lstat %s: %w", rightFilePath, err)
	}
	// Mode
	if leftInfo.Mode() != rightInfo.Mode() {
		return true, nil
	}
	// Symlinks
	if leftInfo.Mode()&os.ModeSymlink != 0 {
		leftTarget, err := os.Readlink(leftFilePath)
		if err != nil {
			return false, fmt.Errorf("readlink %s: %w", leftFilePath, err)
		}
		rightTarget, err := os.Readlink(rightFilePath)
		if err != nil {
			return false, fmt.Errorf("readlink %s: %w", rightFilePath, err)
		}
		return leftTarget != rightTarget, nil
	}
	// Regular files
	if leftInfo.Mode().IsRegular() {
		return cmpIsDiff(leftFilePath, rightFilePath)
	}
	// Devices
	if leftInfo.Mode()&os.ModeDevice != 0 {
		leftSy, ok := leftInfo.Sys().(*syscall.Stat_t)
		if !ok {
			return false, fmt.Errorf("unexpected stat type for %s", leftFilePath)
		}
		rightSys, ok := rightInfo.Sys().(*syscall.Stat_t)
		if !ok {
			return false, fmt.Errorf("unexpected stat type for %s", rightFilePath)
		}

		leftDev := uint64(leftSys.Rdev)
		rightDev := uint64(rightSys.Rdev)

		if unix.Major(leftDev) != unix.Major(rightDev) ||
			unix.Minor(leftDev) != unix.Minor(rightDev) {
			return true, nil
		}
	}
	// TODO: sockets, pipes, other more detailed inspection?
	return false, nil
}

func recurseDir(rootViewDir, containerViewDir string, containerPaths *[]string) error {
	dentries, err := os.ReadDir(rootViewDir)
	if err != nil {
		return fmt.Errorf("read directory %s: %w", rootViewDir, err)
	}
	for _, dentry := range dentries {
		rootViewPath := filepath.Join(rootViewDir, dentry.Name())
		containerViewPath := filepath.Join(containerViewDir, dentry.Name())
		if dentry.IsDir() {
			if err := recurseDir(rootViewPath, containerViewPath, containerPaths); err != nil {
				return err
			}
		} else {
			*containerPaths = append(*containerPaths, containerViewPath)
		}
	}
	return nil
}

func walkFS(leftDir, rightDir, containerViewDir string, deleted, added, modified *[]string) error {
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
			// Left-only entries -> deleted
			rootViewSubpath := filepath.Join(leftDir, name)
			
			if leftDentry.IsDir() {
				if err := recurseDir(rootViewSubpath, containerViewSubpath, deleted); err != nil {
					return err
				}
			} else {
				*deleted = append(*deleted, containerViewSubpath)
			}
			continue
		}
		// Entries in both -> check for modifications
		leftRootViewSubpath := filepath.Join(leftDir, name)
		rightRootViewSubpath := filepath.Join(rightDir, name)
		if leftDentry.IsDir() && rightDentry.IsDir() {
			if err := walkFS(leftRootViewSubpath, rightRootViewSubpath, containerViewSubpath, deleted, added, modified); err != nil {
				return err
			}
			continue
		}
		// if just left is directory, directory contents removed, file added
		if leftDentry.IsDir() && !rightDentry.IsDir() {
			if err := recurseDir(leftRootViewSubpath, containerViewSubpath, deleted); err != nil {
				return err
			}
			*added = append(*added, containerViewSubpath)
			continue
		}
		// if just right is directory, directory contents added, file removed
		if !leftDentry.IsDir() && rightDentry.IsDir() {
			if err := recurseDir(rightRootViewSubpath, containerViewSubpath, added); err != nil {
				return err
			}
			*deleted = append(*deleted, containerViewSubpath)
			continue
		}
		// Neither are directories, check for modification
		changed, err := areNonDirsDiff(leftRootViewSubpath, rightRootViewSubpath)
		if err != nil {
			return err
		}
		if changed {
			*modified = append(*modified, containerViewSubpath)
		}
	}
	// Right-only entries -> added
	for name, dentry := range rightNameMap {
		if _, exists := leftNameMap[name]; exists {
			continue
		}
		rootViewSubpath := filepath.Join(rightDir, name)
		containerViewSubpath := filepath.Join(containerViewDir, name)
		if dentry.IsDir() {
			if err := recurseDir(rootViewSubpath, containerViewSubpath, added); err != nil {
				return err
			}
		} else {
			*added = append(*added, containerViewSubpath)
		}
	}
    return nil
}

func existsDir(path string) (bool, error) {
    info, err := os.Lstat(path) // requires non symlink for the target dir running diff on. change?
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

func normalizeTargetDir(targetDir string) (string, error) {
    // Treat / as relative to the container root.
	if targetDir == "" || targetDir == "/" || targetDir == "." {
        return "", nil
    }
    targetDir = strings.TrimLeft(targetDir, "/")

    clean := filepath.Clean(targetDir)
    if clean == ".." ||
        strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
        return "", fmt.Errorf("target directory must not escape the filesystem root")
    }
    return clean, nil
}

func newDiffRecord(m *Manager, diffDir, leftCkptID, rightCkptID string) (*Diff, error) {
	leftMetadata, err := m.loadMetadata(leftCkptID)
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata for checkpoint %s: %w", leftCkptID, err)
	}
	rightMetadata, err := m.loadMetadata(rightCkptID)
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata for checkpoint %s: %w", rightCkptID, err)
	}
	leftLowerIDs := append(append([]string(nil), leftMetadata.LayerIDs...))
	rightLowerIDs := append(append([]string(nil), rightMetadata.LayerIDs...))

	leftOriginalDir := leftMetadata.OriginalDir
	if leftOriginalDir == "" {
		leftOriginalDir = m.originalDir
	}
	rightOriginalDir := rightMetadata.OriginalDir
	if rightOriginalDir == "" {
		rightOriginalDir = m.originalDir
	}
	rootDir := filepath.Join(diffDir, "overlays")
	return &Diff{
		rootDir:            rootDir,
		leftLayerIDs:       leftLowerIDs,
		rightLayerIDs:      rightLowerIDs,
		leftOriginalDir:	leftOriginalDir,
		rightOriginalDir:	rightOriginalDir,
		leftUpperDir:       filepath.Join(rootDir, "left_upper"),
		rightUpperDir:      filepath.Join(rootDir, "right_upper"),
		leftWorkDir:        filepath.Join(rootDir, "left_work"),
		rightWorkDir:       filepath.Join(rootDir, "right_work"),
		leftMountPoint:     filepath.Join(rootDir, "left_mount"),
		rightMountPoint:    filepath.Join(rootDir, "right_mount"),
		resultsFile:        filepath.Join(diffDir, "results.json"),
	}, nil
}

func computeDiffID(leftCkptID, rightCkptID string) string {
	return fmt.Sprintf("%s-%s", leftCkptID, rightCkptID)
}

func (m *Manager) setupDiffFS(ckptIDFirst, ckptIDSecond string) (*Diff, error) {
	if err := validateCheckpointID(ckptIDFirst); err != nil {
		return nil, fmt.Errorf("invalid checkpoint ID %s: %w", ckptIDFirst, err)
	}
	if err := validateCheckpointID(ckptIDSecond); err != nil {
		return nil, fmt.Errorf("invalid checkpoint ID %s: %w", ckptIDSecond, err)
	}

	// check if already have diff computed
	diffID := computeDiffID(ckptIDFirst, ckptIDSecond)
	diffDir := m.diffDir(diffID)

	var d *Diff
	if err := m.withSessionLock(func() error {
		// TODO first need to check if diffDir/results.json already exists, if so need to indicate that so can just read it.
		if _, err := os.Stat(diffDir); err == nil {
			// TODO
		} else if os.IsNotExist(err) {
			if err := os.MkdirAll(diffDir, 0o755); err != nil {
				return fmt.Errorf("mkdir %s failed: %w", diffDir, err)
			}
		} else {
			return err
		}
		var err error
		d, err = newDiffRecord(m, diffDir, ckptIDFirst, ckptIDSecond)
		if err != nil {
			return err
		}
		// if have the diffDir, and the rootDir, but no results.json, fail (should just retry later)
		if _, err := os.Stat(d.rootDir); err == nil {
			return fmt.Errorf("diff workspace %s already exists but results.json not found", d.rootDir)
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.MkdirAll(d.rootDir, 0o700); err != nil {
			return fmt.Errorf("mkdir %s failed: %w", d.rootDir, err)
		}
		for _, dir := range []string{d.leftMountPoint, d.rightMountPoint, d.leftUpperDir, d.rightUpperDir, d.leftWorkDir, d.rightWorkDir} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("mkdir %s failed: %w", dir, err)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	// Mount
	// layerIDs is ordered oldest to newest, overlayLowerDirs inverts this order to give priority to newest
	leftOverlayLowers := overlayLowerDirs(m.checkpointsDir(), d.leftLayerIDs, d.leftOriginalDir)
	rightOverlayLowers := overlayLowerDirs(m.checkpointsDir(), d.rightLayerIDs, d.rightOriginalDir)

	optionsLeft := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
		strings.Join(leftOverlayLowers, ":"), d.leftUpperDir, d.leftWorkDir)
	if err := unix.Mount("overlay", d.leftMountPoint, "overlay", 0, optionsLeft); err != nil {
		_ = os.RemoveAll(d.rootDir)
		return nil, fmt.Errorf("mount overlay at %s failed: %w", d.leftMountPoint, err)
	}

	optionsRight := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
		strings.Join(rightOverlayLowers, ":"), d.rightUpperDir, d.rightWorkDir)
	if err := unix.Mount("overlay", d.rightMountPoint, "overlay", 0, optionsRight); err != nil {
		_ = unix.Unmount(d.leftMountPoint, unix.MNT_DETACH)
		_ = os.RemoveAll(d.rootDir)
		return nil, fmt.Errorf("mount overlay at %s failed: %w", d.rightMountPoint, err)
	}

	return d, nil
}

// Entry point for diff
func (m *Manager) PrintDiff(ckptIDFirst, ckptIDSecond, targetDir string) (error) {
	d, err := m.setupDiffFS(ckptIDFirst, ckptIDSecond)
	if err != nil {
		return err
	}
	defer func() {
    _ = unix.Unmount(d.rightMountPoint, unix.MNT_DETACH)
    _ = unix.Unmount(d.leftMountPoint, unix.MNT_DETACH)
    _ = os.RemoveAll(d.rootDir)
	}()

	relativeTargetDir, err := normalizeTargetDir(targetDir)
	if err != nil {
		return err
	}

	// TODO: further symlink validation?
	leftTargetDir := filepath.Join(d.leftMountPoint, relativeTargetDir)
	rightTargetDir := filepath.Join(d.rightMountPoint, relativeTargetDir)
	
	leftExists, err := existsDir(leftTargetDir)
	if err != nil {
		return err
	}
	rightExists, err := existsDir(rightTargetDir)
	if err != nil {
		return err
	}
	var added, deleted, modified []string
	switch {
	case !leftExists && !rightExists:
	case leftExists && !rightExists:
		if err := recurseDir(leftTargetDir, relativeTargetDir, &deleted); err != nil {
			return err
		}
	case !leftExists && rightExists:
		if err := recurseDir(rightTargetDir, relativeTargetDir, &added); err != nil {
			return err
		}
	default:
		if err := walkFS(leftTargetDir, rightTargetDir, relativeTargetDir, &deleted, &added, &modified); err != nil {
			return err
		}
	}

	sort.Strings(added)
	sort.Strings(deleted)
	sort.Strings(modified)

	printPaths := func(title string, paths []string) {
		fmt.Println(title + ":")
		for _, path := range paths {
			fmt.Printf("  %s\n", path)

			if ckptIDs, ok := leftBlame[path]; ok && len(ckptIDs) > 0 {
				fmt.Printf("        Left blame:  %s\n", strings.Join(ckptIDs, ", "))
			}
			if ckptIDs, ok := rightBlame[path]; ok && len(ckptIDs) > 0 {
				fmt.Printf("        Right blame: %s\n", strings.Join(ckptIDs, ", "))
			}
		}
	}

	printPaths("Added", added)
	printPaths("Deleted", deleted)
	printPaths("Modified", modified)
	return nil
}