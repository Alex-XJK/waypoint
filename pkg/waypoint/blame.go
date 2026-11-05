package waypoint

// TODO add description

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

type BlameResult struct {
	TargetPath   string `json:"target_path"`
	TargetCkptID string `json:"target_checkpoint_id"`
	// TODO should we output all ckptIDs in the ancestry, with empty blame if necessary?
	Records []BlameCkptRecord `json:"blamed_checkpoints"`
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

type DiffLine struct {
	Prefix byte
	Text   string
}

type diffHunk struct {
	start int
	end   int
}

// constructLineByLine compares two files and returns their unified diff.
func constructLineByLine(prevState, newState *state, change ChangeType) (string, error) {
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
	previousFile, err := os.Open(prevState.rootViewPath)
	if err != nil {
		return "", fmt.Errorf("open file %s: %w", prevState.rootViewPath, err)
	}
	defer previousFile.Close()

	newFile, err := os.Open(newState.rootViewPath)
	if err != nil {
		return "", fmt.Errorf("open file %s: %w", newState.rootViewPath, err)
	}
	defer newFile.Close()

	previousLines, err := readLines(previousFile)
	if err != nil {
		return "", fmt.Errorf("read file %s: %w", prevState.rootViewPath, err)
	}

	newLines, err := readLines(newFile)
	if err != nil {
		return "", fmt.Errorf("read file %s: %w", newState.rootViewPath, err)
	}

	diff := myersDiff(previousLines, newLines)

	return formatUnifiedDiff(prevState.ckptID, newState.ckptID, diff, 3), nil
}

func constructSingleFileDiff(path, prevCkptID, newCkptID string, added bool) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("lstat file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", nil
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open file %s: %w", path, err)
	}
	defer file.Close()
	lines, err := readLines(file)
	if err != nil {
		return "", fmt.Errorf("read file %s: %w", path, err)
	}
	var diff []DiffLine

	prefix := byte('-')
	if added {
		prefix = '+'
	}
	for _, line := range lines {
		diff = append(diff, DiffLine{
			Prefix: prefix,
			Text:   line,
		})
	}

	return formatUnifiedDiff(prevCkptID, newCkptID, diff, 3), nil
}

func readLines(file *os.File) ([]string, error) {
	reader := bufio.NewReader(file)
	var lines []string

	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")
			lines = append(lines, line)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}

	return lines, nil
}

// myersDiff computes the shortest edit script between two sequences of lines.
func myersDiff(oldLines, newLines []string) []DiffLine {
	n := len(oldLines)
	m := len(newLines)
	maxD := n + m

	offset := maxD + 1
	v := make([]int, 2*maxD+3)
	v[offset+1] = 0

	// Save the V array at each edit distance for reconstruction.
	trace := make([][]int, 0, maxD+1)

	foundD := 0

search:
	for d := 0; d <= maxD; d++ {
		trace = append(trace, append([]int(nil), v...))

		for k := -d; k <= d; k += 2 {
			var x int

			if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				x = v[offset+k+1]
			} else {
				x = v[offset+k-1] + 1
			}

			y := x - k

			// Follow the diagonal while lines match.
			for x < n && y < m && oldLines[x] == newLines[y] {
				x++
				y++
			}

			v[offset+k] = x

			if x >= n && y >= m {
				foundD = d
				break search
			}
		}
	}

	// Reconstruct the edit script by walking backwards.
	var reversed []DiffLine

	x, y := n, m

	for d := foundD; d > 0; d-- {
		prevV := trace[d]
		k := x - y

		var prevK int

		if k == -d || (k != d && prevV[offset+k-1] < prevV[offset+k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}

		prevX := prevV[offset+prevK]
		prevY := prevX - prevK

		// Matching lines along the diagonal.
		for x > prevX && y > prevY {
			reversed = append(reversed, DiffLine{
				Prefix: ' ',
				Text:   oldLines[x-1],
			})
			x--
			y--
		}

		// Determine whether the edit was an insertion or deletion.
		if x == prevX {
			reversed = append(reversed, DiffLine{
				Prefix: '+',
				Text:   newLines[y-1],
			})
			y--
		} else {
			reversed = append(reversed, DiffLine{
				Prefix: '-',
				Text:   oldLines[x-1],
			})
			x--
		}
	}

	// Add any remaining common prefix or initial edits.
	for x > 0 && y > 0 {
		reversed = append(reversed, DiffLine{
			Prefix: ' ',
			Text:   oldLines[x-1],
		})
		x--
		y--
	}

	for x > 0 {
		reversed = append(reversed, DiffLine{
			Prefix: '-',
			Text:   oldLines[x-1],
		})
		x--
	}

	for y > 0 {
		reversed = append(reversed, DiffLine{
			Prefix: '+',
			Text:   newLines[y-1],
		})
		y--
	}

	// Restore original order.
	result := make([]DiffLine, len(reversed))
	for i := range reversed {
		result[len(reversed)-1-i] = reversed[i]
	}

	return result
}

func formatUnifiedDiff(prevCkptID, newCkptID string, diff []DiffLine, context int) string {
	var changed []int

	for i, line := range diff {
		if line.Prefix != ' ' {
			changed = append(changed, i)
		}
	}
	if len(changed) == 0 {
		return ""
	}
	// Build hunks by expanding each change with surrounding context.
	var hunks []diffHunk

	for _, idx := range changed {
		start := max(0, idx-context)
		end := min(len(diff), idx+context+1)

		if len(hunks) > 0 && start <= hunks[len(hunks)-1].end {
			if end > hunks[len(hunks)-1].end {
				hunks[len(hunks)-1].end = end
			}
		} else {
			hunks = append(hunks, diffHunk{
				start: start,
				end:   end,
			})
		}
	}

	var result strings.Builder

	fmt.Fprintf(&result, "--- %s\n", prevCkptID)
	fmt.Fprintf(&result, "+++ %s\n", newCkptID)

	oldLine := 0
	newLine := 0
	diffIndex := 0

	for _, hunk := range hunks {
		// Advance line counters to the start of this hunk.
		for diffIndex < hunk.start {
			switch diff[diffIndex].Prefix {
			case ' ':
				oldLine++
				newLine++
			case '-':
				oldLine++
			case '+':
				newLine++
			}
			diffIndex++
		}

		oldCount := 0
		newCount := 0

		for i := hunk.start; i < hunk.end; i++ {
			switch diff[i].Prefix {
			case ' ':
				oldCount++
				newCount++
			case '-':
				oldCount++
			case '+':
				newCount++
			}
		}

		oldRange := formatDiffRange(oldLine, oldCount)
		newRange := formatDiffRange(newLine, newCount)

		fmt.Fprintf(&result, "@@ -%s +%s @@\n", oldRange, newRange)

		for i := hunk.start; i < hunk.end; i++ {
			line := diff[i]

			result.WriteByte(line.Prefix)
			result.WriteString(line.Text)
			result.WriteByte('\n')

			switch line.Prefix {
			case ' ':
				oldLine++
				newLine++
			case '-':
				oldLine++
			case '+':
				newLine++
			}
		}

		diffIndex = hunk.end
	}

	return result.String()
}

func formatDiffRange(start, count int) string {
	if count == 0 {
		return fmt.Sprintf("%d,0", start)
	}

	start++

	if count == 1 {
		return fmt.Sprintf("%d", start)
	}

	return fmt.Sprintf("%d,%d", start, count)
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

func isWhiteout(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
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

func isDescendant(dirPath string, targetPath string) (bool, error) {
	childPath, err := filepath.Rel(dirPath, targetPath)
	if err != nil {
		return false, err
	}
	if childPath == "." || childPath == ".." || strings.HasPrefix(childPath, ".."+string(filepath.Separator)) {
		return false, nil
	}
	return true, nil
}

func insertBlameRecord(blamed *[]BlameCkptRecord, prevState, newState *state, changeType ChangeType) error {
	if blamed == nil {
		return nil
	}
	diff, err := constructLineByLine(prevState, newState, changeType)
	if err != nil {
		return fmt.Errorf("construct diff for checkpoints %s, %s: %w", prevState.ckptID, newState.ckptID, err)
	}
	lines := strings.Split(strings.TrimSuffix(diff, "\n"), "\n")

	*blamed = append(*blamed, BlameCkptRecord{
		CheckpointID: newState.ckptID,
		ChangeType:   changeType,
		LineByLine:   lines,
	})
	return nil
}

func checkDir(rootViewDir, containerViewDir, targetPath string, prevState *state, blamed *[]BlameCkptRecord, ckptID string) (bool, error) {
	dentries, err := os.ReadDir(rootViewDir)
	if err != nil {
		return false, fmt.Errorf("read directory %s: %w", rootViewDir, err)
	}
	for _, dentry := range dentries {
		rootViewPath := filepath.Join(rootViewDir, dentry.Name())
		containerViewPath := filepath.Join(containerViewDir, dentry.Name())
		if !dentry.IsDir() { //TODO won't follow symlinks, change?
			whiteout, err := isWhiteout(rootViewPath)
			if err != nil {
				return false, fmt.Errorf("check whiteout %s: %w", rootViewPath, err)
			}
			if whiteout {
				if prevState.visible {
					if containerViewPath == targetPath { // whiteout of target file
						newState := &state{
							visible:      false,
							rootViewPath: "",
							ckptID:       ckptID,
						}
						if err := insertBlameRecord(blamed, prevState, newState, ChangeTypeDeleted); err != nil {
							return false, err
						}
						*prevState = *newState
						return true, nil
					} else { // whiteout could represent directory, if so check if target is child
						hide, err := isDescendant(containerViewPath, targetPath)
						if err != nil {
							return false, err
						}
						if hide {
							newState := &state{
								visible:      false,
								rootViewPath: "",
								ckptID:       ckptID,
							}
							if err := insertBlameRecord(blamed, prevState, newState, ChangeTypeDeleted); err != nil {
								return false, err
							}
							*prevState = *newState
							return true, nil
						}
					}
				}
			} else if containerViewPath == targetPath {
				newState := &state{
					visible:      true,
					rootViewPath: rootViewPath,
					ckptID:       ckptID,
				}
				if !prevState.visible {
					if err := insertBlameRecord(blamed, prevState, newState, ChangeTypeAdded); err != nil {
						return false, err
					}
				} else {
					if err := insertBlameRecord(blamed, prevState, newState, ChangeTypeModified); err != nil {
						return false, err
					}
				}
				*prevState = *newState
				return true, nil
			}
		} else {
			relevantDir, err := isDescendant(containerViewPath, targetPath)
			if err != nil {
				return false, err
			}
			if !relevantDir {
				continue
			}
			updated, err := checkDir(rootViewPath, containerViewPath, targetPath, prevState, blamed, ckptID)
			if err != nil {
				return false, err
			}
			if updated {
				return true, nil
			}
			opaque, err := isOpaque(rootViewPath)
			if err != nil {
				return false, fmt.Errorf("check opacity %s: %w", rootViewPath, err)
			}
			if opaque && prevState.visible {
				newState := &state{
					visible:      false,
					rootViewPath: "",
					ckptID:       ckptID,
				}
				if err := insertBlameRecord(blamed, prevState, newState, ChangeTypeDeleted); err != nil {
					return false, err
				}
				*prevState = *newState
				return true, nil
			}
		}
	}
	return false, nil
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
	_, err = checkDir(originalDir, "/", normalizedTargetPath, state, nil, "original")
	if err != nil {
		return err
	}
	// then advance through checkpoint layers, blaming
	for _, ckptID := range ckptIDs {
		upperDirPath := m.checkpointUpperDir(ckptID)
		_, err := checkDir(upperDirPath, "/", normalizedTargetPath, state, &blamedCkpts, ckptID)
		if err != nil {
			return err
		}
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
