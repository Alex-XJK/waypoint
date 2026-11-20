package waypoint

// TODO add description

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

// blameDiffBudgetBytes bounds the file content that diffs running at the same
// time hold in memory.
const blameDiffBudgetBytes = 64 << 20

// diffBudget admits diffs while their inputs fit in blameDiffBudgetBytes. A
// diff larger than the budget runs alone.
type diffBudget struct {
	mu     sync.Mutex
	freed  *sync.Cond
	inUse  int64
	active int
}

func newDiffBudget() *diffBudget {
	b := &diffBudget{}
	b.freed = sync.NewCond(&b.mu)
	return b
}

// acquire waits until a diff reading size bytes may run and returns the
// function that ends it.
func (b *diffBudget) acquire(size int64) (release func()) {
	b.mu.Lock()
	for b.active > 0 && b.inUse+size > blameDiffBudgetBytes {
		b.freed.Wait()
	}
	b.inUse += size
	b.active++
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		b.inUse -= size
		b.active--
		b.mu.Unlock()
		b.freed.Broadcast()
	}
}

// regularSize returns the size of path and whether it is a regular file.
func regularSize(path string) (int64, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, false, fmt.Errorf("lstat file %s: %w", path, err)
	}
	return info.Size(), info.Mode().IsRegular(), nil
}

// pathsDiffer reports whether two regular files differ in content.
func pathsDiffer(prevPath, nextPath string, bufs *compareBufs) (bool, error) {
	prevFile, err := os.Open(prevPath)
	if err != nil {
		return false, fmt.Errorf("open file %s: %w", prevPath, err)
	}
	defer prevFile.Close()
	nextFile, err := os.Open(nextPath)
	if err != nil {
		return false, fmt.Errorf("open file %s: %w", nextPath, err)
	}
	defer nextFile.Close()
	return openFilesDiffer(prevFile, nextFile, nextPath, bufs)
}

// readFileString reads a file into a string without a second copy.
func readFileString(path string, size int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read file %s: %w", path, err)
	}
	defer file.Close()
	var content strings.Builder
	content.Grow(int(size))
	if _, err := io.Copy(&content, file); err != nil {
		return "", fmt.Errorf("read file %s: %w", path, err)
	}
	return content.String(), nil
}

// buildBlameRecord turns an event into its record, with the change as
// unified-diff lines. A layer that holds a regular file with unchanged
// content changed only its metadata.
func buildBlameRecord(event *blameEvent, bufs *compareBufs, budget *diffBudget) (BlameCkptRecord, error) {
	record := BlameCkptRecord{CheckpointID: event.next.ckptID, ChangeType: event.change, LineByLine: []string{}}
	fail := func(err error) (BlameCkptRecord, error) {
		return BlameCkptRecord{}, fmt.Errorf("construct diff for checkpoints %s, %s: %w", event.prev.ckptID, event.next.ckptID, err)
	}

	var prevSize, nextSize int64
	var prevRegular, nextRegular bool
	var err error
	if event.change != ChangeTypeAdded {
		if prevSize, prevRegular, err = regularSize(event.prev.rootViewPath); err != nil {
			return fail(err)
		}
	}
	if event.change != ChangeTypeDeleted {
		if nextSize, nextRegular, err = regularSize(event.next.rootViewPath); err != nil {
			return fail(err)
		}
	}
	switch event.change {
	case ChangeTypeAdded:
		if !nextRegular {
			return record, nil
		}
	case ChangeTypeDeleted:
		if !prevRegular {
			return record, nil
		}
	default:
		if !prevRegular || !nextRegular {
			return record, nil
		}
		differ, err := pathsDiffer(event.prev.rootViewPath, event.next.rootViewPath, bufs)
		if err != nil {
			return fail(err)
		}
		if !differ {
			record.ChangeType = ChangeTypeMetadata
			return record, nil
		}
	}

	defer budget.acquire(prevSize + nextSize)()
	var prevText, nextText string
	if prevRegular {
		if prevText, err = readFileString(event.prev.rootViewPath, prevSize); err != nil {
			return fail(err)
		}
	}
	if nextRegular {
		if nextText, err = readFileString(event.next.rootViewPath, nextSize); err != nil {
			return fail(err)
		}
	}
	if diff := udiff.Unified(event.prev.ckptID, event.next.ckptID, prevText, nextText); diff != "" {
		record.LineByLine = strings.Split(strings.TrimSuffix(diff, "\n"), "\n")
	}
	return record, nil
}

// buildBlameRecords builds the records of events, in order, on up to
// compareWorkers goroutines.
func buildBlameRecords(events []*blameEvent) ([]BlameCkptRecord, error) {
	records := make([]BlameCkptRecord, len(events))
	errs := make([]error, len(events))
	budget := newDiffBudget()
	var next atomic.Int64
	var failed atomic.Bool
	var wg sync.WaitGroup
	for range min(compareWorkers, len(events)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bufs := newCompareBufs()
			for !failed.Load() {
				i := int(next.Add(1)) - 1
				if i >= len(events) {
					return
				}
				if records[i], errs[i] = buildBlameRecord(events[i], bufs, budget); errs[i] != nil {
					failed.Store(true)
				}
			}
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return records, nil
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
	// then advance through checkpoint layers, collecting what each did to the target
	var events []*blameEvent
	for _, ckptID := range ckptIDs {
		event, err := advance(state, m.checkpointUpperDir(ckptID), ckptID, normalizedTargetPath)
		if err != nil {
			return err
		}
		if event != nil {
			events = append(events, event)
		}
	}
	blamedCkpts, err := buildBlameRecords(events)
	if err != nil {
		return err
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
