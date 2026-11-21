package waypoint

// Line-level blame and blame of many paths.
//
// The blame walk (checkDir, blame.go) follows one path from the original rootfs
// through each layer of a checkpoint's chain and records every change. Line-level
// blame reuses that walk to collect the path's versions, then diffs consecutive
// versions line by line (matchLines) to carry each line's origin forward, as
// git blame does: the result annotates every line of the target version with the
// checkpoint that introduced it, or "original" for lines of the base rootfs.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// blameOriginID names the base rootfs as the origin of a line or version.
const blameOriginID = "original"

// binarySniffLen is how much of a file is searched for a NUL byte to call it binary (as git does).
const binarySniffLen = 8000

// BlameVersion is one change of the target path along the chain.
type BlameVersion struct {
	CheckpointID string     `json:"checkpoint_id"`
	ChangeType   ChangeType `json:"change_type"`
}

// BlameLineRun is a run of consecutive lines of the target version that one checkpoint introduced.
type BlameLineRun struct {
	CheckpointID string   `json:"checkpoint_id"`
	Start        int      `json:"start"` // 1-based number of the run's first line in the target version
	Text         []string `json:"text"`  // the lines without their "\n" terminators
}

// BlameLinesResult annotates each line of a path's version at a checkpoint with the
// checkpoint that introduced it.
type BlameLinesResult struct {
	TargetPath   string         `json:"target_path"`
	TargetCkptID string         `json:"target_checkpoint_id"`
	Versions     []BlameVersion `json:"versions"`
	// Exists is false when the path is absent at the target checkpoint.
	Exists   bool     `json:"exists"`
	FileType FileType `json:"file_type,omitempty"`
	// Binary is true when the target version has a NUL byte in its first 8000 bytes; it has no lines.
	Binary bool `json:"binary,omitempty"`
	// Approximate is true when a version pair exceeded the exact diff's budget and was
	// matched by go-udiff's bounded diff, which can date unchanged lines too late.
	Approximate bool           `json:"approximate,omitempty"`
	LineCount   int            `json:"line_count"`
	Origins     map[string]int `json:"origins"`
	Lines       []BlameLineRun `json:"lines"`
}

// blameError is the entry of a many-path blame for a path that failed.
type blameError struct {
	TargetPath string `json:"target_path"`
	Error      string `json:"error"`
}

// blameChain is the resolved layer chain of the checkpoint a blame starts from.
type blameChain struct {
	targetCkptID string
	originalDir  string
	layerIDs     []string
}

func (m *Manager) loadBlameChain(ckptID string) (*blameChain, error) {
	if err := validateCheckpointID(ckptID); err != nil {
		return nil, fmt.Errorf("invalid checkpoint ID %s: %w", ckptID, err)
	}
	metadata, err := m.loadMetadata(ckptID)
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata for checkpoint %s: %w", ckptID, err)
	}
	if metadata.Status != CheckpointStatusReady {
		return nil, fmt.Errorf("checkpoint %s is not ready (status: %s)", ckptID, metadata.Status)
	}
	return &blameChain{
		targetCkptID: ckptID,
		originalDir:  metadata.OriginalDir,
		layerIDs:     append([]string(nil), metadata.LayerIDs...),
	}, nil
}

// walk follows target (normalized) from the original rootfs through each layer.
// blamed, if not nil, receives a record with a unified diff for each change.
// onVersion, if not nil, is called with the path's state in the original rootfs
// when present there, and after each layer that changed it.
func (c *blameChain) walk(m *Manager, target string, blamed *[]BlameCkptRecord, onVersion func(state)) error {
	st := &state{visible: false, rootViewPath: "", ckptID: blameOriginID}
	if _, err := checkDir(c.originalDir, "/", target, st, nil, blameOriginID); err != nil {
		return err
	}
	if onVersion != nil && st.visible {
		onVersion(*st)
	}
	for _, id := range c.layerIDs {
		if _, err := checkDir(m.checkpointUpperDir(id), "/", target, st, blamed, id); err != nil {
			return err
		}
		if onVersion != nil && st.ckptID == id {
			onVersion(*st)
		}
	}
	return nil
}

func (c *blameChain) blame(m *Manager, path string) (*BlameResult, error) {
	target, err := normalizePath(path)
	if err != nil {
		return nil, err
	}
	records := make([]BlameCkptRecord, 0)
	if err := c.walk(m, target, &records, nil); err != nil {
		return nil, err
	}
	return &BlameResult{TargetPath: target, TargetCkptID: c.targetCkptID, Records: records}, nil
}

// lineOrigin is a line of the current version and the checkpoint that introduced it.
type lineOrigin struct {
	text   string
	origin string
}

func (c *blameChain) blameLines(m *Manager, path string) (*BlameLinesResult, error) {
	target, err := normalizePath(path)
	if err != nil {
		return nil, err
	}
	res := &BlameLinesResult{
		TargetPath:   target,
		TargetCkptID: c.targetCkptID,
		Versions:     []BlameVersion{},
		Origins:      map[string]int{},
		Lines:        []BlameLineRun{},
	}
	var (
		cur     []lineOrigin // lines of the latest text version; nil when the path is absent or not text
		walkErr error
		present bool
	)
	onVersion := func(st state) {
		if walkErr != nil {
			return
		}
		change := ChangeTypeModified
		switch {
		case !st.visible:
			change = ChangeTypeDeleted
		case !present:
			change = ChangeTypeAdded
		}
		if st.ckptID != blameOriginID {
			res.Versions = append(res.Versions, BlameVersion{CheckpointID: st.ckptID, ChangeType: change})
		}
		present = st.visible
		res.Binary, res.FileType = false, ""
		if !st.visible {
			cur = nil
			return
		}
		text, fileType, binary, err := readBlameText(st.rootViewPath)
		if err != nil {
			walkErr = err
			return
		}
		res.FileType, res.Binary = fileType, binary
		if fileType != FileTypeRegular || binary {
			cur = nil
			return
		}
		lines := splitLines(text)
		next := make([]lineOrigin, len(lines))
		if cur == nil {
			for i, line := range lines {
				next[i] = lineOrigin{text: line, origin: st.ckptID}
			}
		} else {
			prev := make([]string, len(cur))
			for i := range cur {
				prev[i] = cur[i].text
			}
			match, exact := matchLines(prev, lines)
			if !exact {
				res.Approximate = true
			}
			for i, line := range lines {
				origin := st.ckptID
				if match[i] >= 0 {
					origin = cur[match[i]].origin
				}
				next[i] = lineOrigin{text: line, origin: origin}
			}
		}
		cur = next
	}
	if err := c.walk(m, target, nil, onVersion); err != nil {
		return nil, err
	}
	if walkErr != nil {
		return nil, walkErr
	}
	res.Exists = present
	res.LineCount = len(cur)
	for i, line := range cur {
		res.Origins[line.origin]++
		text := strings.TrimSuffix(line.text, "\n")
		if n := len(res.Lines); n > 0 && res.Lines[n-1].CheckpointID == line.origin {
			res.Lines[n-1].Text = append(res.Lines[n-1].Text, text)
			continue
		}
		res.Lines = append(res.Lines, BlameLineRun{CheckpointID: line.origin, Start: i + 1, Text: []string{text}})
	}
	return res, nil
}

// readBlameText reads one version of the target. Non-regular files and binary
// content are reported without their text.
func readBlameText(path string) (text string, fileType FileType, binary bool, err error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", "", false, fmt.Errorf("lstat file %s: %w", path, err)
	}
	switch mode := info.Mode(); {
	case mode.IsRegular():
		fileType = FileTypeRegular
	case mode&os.ModeSymlink != 0:
		return "", FileTypeSymlink, false, nil
	default:
		return "", FileTypeOther, false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", false, fmt.Errorf("read file %s: %w", path, err)
	}
	if bytes.IndexByte(data[:min(len(data), binarySniffLen)], 0) >= 0 {
		return "", fileType, true, nil
	}
	return string(data), fileType, false, nil
}

// PrintBlameMany blames each path from one checkpoint and prints a JSON array with
// one entry per path, in order. A path that fails gets an entry with its error
// instead of failing the whole call. With lines, each entry is a line-level blame.
func (m *Manager) PrintBlameMany(ckptID string, paths []string, lines bool, out io.Writer) error {
	chain, err := m.loadBlameChain(ckptID)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(out, "["); err != nil {
		return err
	}
	for i, path := range paths {
		var item any
		var err error
		if lines {
			item, err = chain.blameLines(m, path)
		} else {
			item, err = chain.blame(m, path)
		}
		if err != nil {
			item = blameError{TargetPath: path, Error: err.Error()}
		}
		data, err := json.MarshalIndent(item, "  ", "  ")
		if err != nil {
			return fmt.Errorf("encode blame of %s: %w", path, err)
		}
		sep := ",\n  "
		if i == 0 {
			sep = "\n  "
		}
		if _, err := io.WriteString(out, sep); err != nil {
			return err
		}
		if _, err := out.Write(data); err != nil {
			return err
		}
	}
	tail := "]\n"
	if len(paths) > 0 {
		tail = "\n]\n"
	}
	_, err = io.WriteString(out, tail)
	return err
}

// PrintBlameLines prints the line-level blame of one path as a JSON object.
func (m *Manager) PrintBlameLines(ckptID, path string) error {
	chain, err := m.loadBlameChain(ckptID)
	if err != nil {
		return err
	}
	res, err := chain.blameLines(m, path)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		return fmt.Errorf("encode JSON to stdout: %w", err)
	}
	return nil
}
