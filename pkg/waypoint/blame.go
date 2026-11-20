package waypoint

// TODO add description

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/aymanbagabas/go-udiff"
	"golang.org/x/sys/unix"
)

type BlameResult struct {
	TargetPath   string            `json:"target_path"`
	TargetCkptID string            `json:"target_checkpoint_id"`
	Records      []BlameCkptRecord `json:"blamed_checkpoints"`
}

type BlameCkptRecord struct {
	CheckpointID string     `json:"checkpoint_id"`
	ChangeType   ChangeType `json:"change_type"`
	LineByLine   []string   `json:"line_by_line_diff"`
}

type state struct {
	visible      bool
	rootViewPath string
	ckptID       string
}

// constructFileDiff compares two files and returns their unified diff.
func constructFileDiff(prevState, newState *state, change ChangeType) (string, error) {
	if change == ChangeTypeAdded {
		return constructSingleFileDiff(newState.rootViewPath, prevState.ckptID, newState.ckptID, true)
	}
	if change == ChangeTypeDeleted {
		return constructSingleFileDiff(prevState.rootViewPath, prevState.ckptID, newState.ckptID, false)
	}
	for _, path := range []string{prevState.rootViewPath, newState.rootViewPath} {
		info, err := os.Lstat(path)
		if err != nil {
			return "", fmt.Errorf("lstat file %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return "", nil
		}
	}

	previousContent, err := os.ReadFile(prevState.rootViewPath)
	if err != nil {
		return "", fmt.Errorf("read file %s: %w", prevState.rootViewPath, err)
	}
	newContent, err := os.ReadFile(newState.rootViewPath)
	if err != nil {
		return "", fmt.Errorf("read file %s: %w", newState.rootViewPath, err)
	}
	diff := udiff.Unified(prevState.ckptID, newState.ckptID, string(previousContent), string(newContent))

	return diff, nil
}

func constructSingleFileDiff(path, prevCkptID, newCkptID string, added bool) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("lstat file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read file %s: %w", path, err)
	}
	if added {
		return udiff.Unified(prevCkptID, newCkptID, "", string(content)), nil
	}
	return udiff.Unified(prevCkptID, newCkptID, string(content), ""), nil
}

func isOpaque(dir string) (bool, error) {
	value := make([]byte, 10)
	n, err := unix.Getxattr(dir, "trusted.overlay.opaque", value)
	if err != nil {
		if errors.Is(err, unix.ENODATA) {
			return false, nil // Xattr doesn't exist; not opaque.
		}
		return false, fmt.Errorf("get opaque xattr for %s: %w", dir, err)
	}

	return string(value[:n]) == "y", nil
}

// isWhiteoutInfo reports whether path, with the given Lstat result, is an
// OverlayFS whiteout.
func isWhiteoutInfo(path string, info os.FileInfo) (bool, error) {
	// Whiteout represented as a character device with major/minor 0/0.
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		if info.Mode()&os.ModeCharDevice != 0 &&
			int(unix.Major(uint64(stat.Rdev))) == 0 &&
			int(unix.Minor(uint64(stat.Rdev))) == 0 {
			return true, nil
		}
	}
	// Whiteout represented as a zero-size regular file with xattr.
	if info.Mode().IsRegular() && info.Size() == 0 {
		_, err := unix.Getxattr(path, "trusted.overlay.whiteout", nil)
		if err != nil {
			if errors.Is(err, unix.ENODATA) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func areRegularFiles(paths ...string) (bool, error) {
	for _, path := range paths {
		info, err := os.Lstat(path)
		if err != nil {
			return false, fmt.Errorf("lstat file %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return false, nil
		}
	}
	return true, nil
}

// buildBlameRecord turns an event into its record, with the change as
// unified-diff lines.
func buildBlameRecord(event *blameEvent) (BlameCkptRecord, error) {
	changeType := event.change
	diff, err := constructFileDiff(&event.prev, &event.next, changeType)
	if err != nil {
		return BlameCkptRecord{}, fmt.Errorf("construct diff for checkpoints %s, %s: %w", event.prev.ckptID, event.next.ckptID, err)
	}
	lines := []string{}
	if diff != "" {
		lines = strings.Split(strings.TrimSuffix(diff, "\n"), "\n")
	} else if changeType == ChangeTypeModified {
		// A layer that holds a regular file with unchanged content changed only its metadata.
		regular, err := areRegularFiles(event.prev.rootViewPath, event.next.rootViewPath)
		if err != nil {
			return BlameCkptRecord{}, err
		}
		if regular {
			changeType = ChangeTypeMetadata
		}
	}

	return BlameCkptRecord{
		CheckpointID: event.next.ckptID,
		ChangeType:   changeType,
		LineByLine:   lines,
	}, nil
}

// layerEntry is what one layer holds at the target path.
type layerEntry int

const (
	layerUntouched layerEntry = iota // nothing at the target path
	layerHidden                      // a whiteout, an opaque directory or a non-directory on the path, or a directory at it
	layerPresent                     // a file, symlink or other non-directory at the target path
)

// lookupLayer reports what the layer rooted at layerRoot holds at targetPath,
// and the entry's host path for layerPresent. It walks the path one component
// at a time and stops at the first that is not a directory, so it follows no
// symlink.
func lookupLayer(layerRoot, targetPath string) (layerEntry, string, error) {
	components := strings.Split(strings.TrimPrefix(targetPath, "/"), "/")
	path := layerRoot
	var ancestors []string // the layer's directories above the target, outermost first
	for i, name := range components {
		path = filepath.Join(path, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			// An opaque directory above the missing entry hides the lower layers.
			for j := len(ancestors) - 1; j >= 0; j-- {
				opaque, err := isOpaque(ancestors[j])
				if err != nil {
					return layerUntouched, "", fmt.Errorf("check opacity %s: %w", ancestors[j], err)
				}
				if opaque {
					return layerHidden, "", nil
				}
			}
			return layerUntouched, "", nil
		}
		if err != nil {
			return layerUntouched, "", fmt.Errorf("lstat %s: %w", path, err)
		}

		last := i == len(components)-1
		if info.IsDir() {
			if last {
				return layerHidden, "", nil
			}
			ancestors = append(ancestors, path)
			continue
		}
		if !last {
			return layerHidden, "", nil
		}
		whiteout, err := isWhiteoutInfo(path, info)
		if err != nil {
			return layerUntouched, "", fmt.Errorf("check whiteout %s: %w", path, err)
		}
		if whiteout {
			return layerHidden, "", nil
		}
		return layerPresent, path, nil
	}
	return layerUntouched, "", nil
}

// blameEvent is a change that one layer makes to the target path.
type blameEvent struct {
	prev, next state
	change     ChangeType
}

// advance applies the layer rooted at layerRoot to st, the target's state
// below that layer, and returns the change the layer makes, or nil.
func advance(st *state, layerRoot, ckptID, targetPath string) (*blameEvent, error) {
	entry, hostPath, err := lookupLayer(layerRoot, targetPath)
	if err != nil {
		return nil, err
	}
	var next state
	var change ChangeType
	switch {
	case entry == layerPresent && st.visible:
		next, change = state{visible: true, rootViewPath: hostPath, ckptID: ckptID}, ChangeTypeModified
	case entry == layerPresent:
		next, change = state{visible: true, rootViewPath: hostPath, ckptID: ckptID}, ChangeTypeAdded
	case entry == layerHidden && st.visible:
		next, change = state{ckptID: ckptID}, ChangeTypeDeleted
	default:
		return nil, nil
	}
	event := &blameEvent{prev: *st, next: next, change: change}
	*st = next
	return event, nil
}

// very similar to diff.go normalizeTargetDir, refactor?
func normalizePath(targetDir string) (string, error) {
	targetDir = strings.TrimLeft(targetDir, "/")
	clean := filepath.Clean(targetDir)

	if clean == "." {
		return "", fmt.Errorf("path is for root directory, blame does not support directory targets") // only diff to normalizeTargetDir
	}
	if clean == ".." ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path must not escape the filesystem root")
	}
	return "/" + clean, nil
}

// Entry point for blame
func (m *Manager) PrintBlame(ckptID, path string) error {
	if err := validateCheckpointID(ckptID); err != nil {
		return fmt.Errorf("invalid checkpoint ID %s: %w", ckptID, err)
	}
	metadata, err := m.loadMetadata(ckptID)
	if err != nil {
		return fmt.Errorf("failed to read metadata for checkpoint %s: %w", ckptID, err)
	}
	if metadata.Status != CheckpointStatusReady {
		return fmt.Errorf("checkpoint %s is not ready (status: %s)", ckptID, metadata.Status)
	}
	normalizedTargetPath, err := normalizePath(path)
	if err != nil {
		return err
	}

	blamedCkpts := make([]BlameCkptRecord, 0)

	ckptIDs := append([]string(nil), metadata.LayerIDs...)
	originalDir := metadata.OriginalDir
	// first check through original dir to set up visibility of target path
	state := &state{
		visible:      false,
		rootViewPath: "",
		ckptID:       "original",
	}
	if _, err := advance(state, originalDir, "original", normalizedTargetPath); err != nil {
		return err
	}
	// then advance through checkpoint layers, blaming
	for _, ckptID := range ckptIDs {
		event, err := advance(state, m.checkpointUpperDir(ckptID), ckptID, normalizedTargetPath)
		if err != nil {
			return err
		}
		if event == nil {
			continue
		}
		record, err := buildBlameRecord(event)
		if err != nil {
			return err
		}
		blamedCkpts = append(blamedCkpts, record)
	}

	result := BlameResult{
		TargetPath:   normalizedTargetPath,
		TargetCkptID: ckptID,
		Records:      blamedCkpts,
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		return fmt.Errorf("encode JSON to stdout: %w", err)
	}

	return nil
}
