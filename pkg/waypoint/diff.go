package waypoint

// TODO add description

import (
	"fmt"
	"os"
	"path/filepath"
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

func computeDiffID(leftCkptID, rightCkptID string) string {
	return fmt.Sprintf("%s-%s", leftCkptID, rightCkptID)
}

func newDiffRecord(m *Manager, diffDir, leftCkptID, rightCkptID string) (*Diff, error) {
	leftMetadata, err := m.readCheckpointMetadata(leftCkptID)
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata for checkpoint %s: %w", leftCkptID, err)
	}
	rightMetadata, err := m.readCheckpointMetadata(rightCkptID)
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata for checkpoint %s: %w", rightCkptID, err)
	}
	leftLowerIDs := append(append([]string(nil), leftMetadata.LayerIDs), leftCkptID)
	rightLowerIDs := append(append([]string(nil), rightMetadata.LayerIDs), rightCkptID)

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
		if err := os.Mkdir(d.rootDir, 0o700); err != nil {
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
		return nil, fmt.Errorf("mount overlay at %s failed: %w", d.leftMountPoint, err)
	}

	optionsRight := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
		strings.Join(rightOverlayLowers, ":"), d.rightUpperDir, d.rightWorkDir)
	if err := unix.Mount("overlay", d.rightMountPoint, "overlay", 0, optionsRight); err != nil {
		return nil, fmt.Errorf("mount overlay at %s failed: %w", d.rightMountPoint, err)
	}	

	return d, nil
}

// Entry point for diff
func (m *Manager) printDiff(ckptIDFirst, ckptIDSecond, targetDir string) (error) {
	d, err := m.setupDiffFS(ckptIDFirst, ckptIDSecond)
	if err != nil {
		return err
	}
	defer func() {
    _ = unix.Unmount(d.rightMountPoint, unix.MNT_DETACH)
    _ = unix.Unmount(d.leftMountPoint, unix.MNT_DETACH)
    _ = os.RemoveAll(d.rootDir)
	}()

	var added, deleted, modified []string


	return nil
}
