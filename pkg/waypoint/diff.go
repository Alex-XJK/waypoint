package waypoint

// `waypoint diff`: what differs between two checkpoints, in the same session
// or in two sessions. The filesystem part is in fsdiff.go, the process part
// in procstate.go. Both read only sealed checkpoint data (upper layers,
// original rootfs, CRIU images), so a diff needs no fork, mount or lock
// beyond the shared images lock that a restore also takes.

import (
	"fmt"
	"path/filepath"
	"strings"
)

const DiffSchema = "waypoint.diff/v1"

// CheckpointDiff is the JSON that `waypoint diff` prints.
type CheckpointDiff struct {
	Schema      string          `json:"schema"`
	From        CheckpointRef   `json:"from"`
	To          CheckpointRef   `json:"to"`
	Filesystem  *FilesystemDiff `json:"filesystem,omitempty"`
	Processes   *ProcessDiff    `json:"processes,omitempty"`
	NotCompared []string        `json:"not_compared"`
}

type CheckpointRef struct {
	Session    string `json:"session"`
	Checkpoint string `json:"checkpoint"`
}

type DiffOptions struct {
	Filesystem bool
	Processes  bool
	// PatchMaxBytes > 0 adds a unified line diff to each record of a text
	// file whose two sides are at most that large.
	PatchMaxBytes int64
	// PatchWords marks changed words inline instead of -/+ lines.
	PatchWords bool
}

// DiffCheckpoints compares checkpoint fromID of session from with
// checkpoint toID of session to (which may be the same manager).
func DiffCheckpoints(from *Manager, fromID string, to *Manager, toID string, opts DiffOptions) (*CheckpointDiff, error) {
	fromMD, err := from.readyCheckpoint(fromID)
	if err != nil {
		return nil, err
	}
	toMD, err := to.readyCheckpoint(toID)
	if err != nil {
		return nil, err
	}
	d := &CheckpointDiff{
		Schema: DiffSchema,
		From:   CheckpointRef{Session: from.sessionID, Checkpoint: fromID},
		To:     CheckpointRef{Session: to.sessionID, Checkpoint: toID},
		NotCompared: []string{
			"file timestamps", "extended attributes", "hard link identity",
			"memory contents other than argv and environment", "shell variables",
			"namespaces, mounts and timers",
		},
	}
	if opts.Filesystem {
		fs, err := diffCheckpointFilesystems(from, fromMD, to, toMD, patchOptions{MaxBytes: opts.PatchMaxBytes, Words: opts.PatchWords})
		if err != nil {
			return nil, err
		}
		d.Filesystem = fs
	}
	if opts.Processes {
		a, err := from.CheckpointProcessState(fromID)
		if err != nil {
			return nil, err
		}
		b, err := to.CheckpointProcessState(toID)
		if err != nil {
			return nil, err
		}
		d.Processes = diffProcessStates(a, b)
	}
	return d, nil
}

func (m *Manager) readyCheckpoint(id string) (*Metadata, error) {
	if err := validateCheckpointID(id); err != nil {
		return nil, err
	}
	md, err := m.loadMetadata(id)
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata for checkpoint %s in session %s: %w", id, m.sessionID, err)
	}
	if md.Status != CheckpointStatusReady {
		return nil, fmt.Errorf("checkpoint %s is not ready (status: %s)", id, md.Status)
	}
	return md, nil
}

// view builds a checkpoint's layer stack, top first.
func (m *Manager) view(md *Metadata) *fsView {
	v := &fsView{hashes: map[string]string{}}
	for i := len(md.LayerIDs) - 1; i >= 0; i-- {
		v.layers = append(v.layers, fsLayer{id: md.LayerIDs[i], dir: m.checkpointUpperDir(md.LayerIDs[i])})
	}
	orig := md.OriginalDir
	if orig == "" {
		orig = m.originalDir
	}
	v.layers = append(v.layers, fsLayer{id: originalLayerID, dir: orig})
	return v
}

func diffCheckpointFilesystems(from *Manager, fromMD *Metadata, to *Manager, toMD *Metadata, popts patchOptions) (*FilesystemDiff, error) {
	va, vb := from.view(fromMD), to.view(toMD)
	internal := internalPaths(from, to)
	skip := func(p string) bool {
		for _, q := range internal {
			if isUnder(p, q) {
				return true
			}
		}
		return false
	}
	// The directories above the session directory are Waypoint's too, but
	// only as entries; what else they hold is the workload's (e.g. /tmp).
	drop := func(p string) bool {
		return isSessionDirAncestor(p, from.baseDir) || isSessionDirAncestor(p, to.baseDir)
	}
	out := &FilesystemDiff{Excluded: internal}
	if popts.MaxBytes > 0 {
		out.PatchFormat = "unified"
		if popts.Words {
			out.PatchFormat = "word"
		}
	}

	shared := 0
	var baseDiffs []string
	origA, origB := va.layers[len(va.layers)-1].dir, vb.layers[len(vb.layers)-1].dir
	if filepath.Clean(origA) == filepath.Clean(origB) {
		// The longest common prefix of the two chains, plus the original.
		n := 0
		for n < len(fromMD.LayerIDs) && n < len(toMD.LayerIDs) && from.baseDir == to.baseDir &&
			fromMD.LayerIDs[n] == toMD.LayerIDs[n] {
			n++
		}
		shared = n + 1
		out.Base = FSBase{Kind: "shared_layers", SharedLayers: append([]string{}, fromMD.LayerIDs[:n]...)}
	} else {
		pa, da, err := from.originalManifest()
		if err != nil {
			return nil, err
		}
		pb, db, err := to.originalManifest()
		if err != nil {
			return nil, err
		}
		out.Base = FSBase{Kind: "equal_originals", FromOriginalDigest: da, ToOriginalDigest: db}
		if da != db {
			out.Base.Kind = "different_originals"
			if baseDiffs, err = manifestDiffs(pa, pb); err != nil {
				return nil, err
			}
		}
	}
	out.FromLayers = append([]string{}, fromMD.LayerIDs[max(shared-1, 0):]...)
	out.ToLayers = append([]string{}, toMD.LayerIDs[max(shared-1, 0):]...)

	records, err := diffFilesystems(va, vb, shared, baseDiffs, skip, drop, popts)
	if err != nil {
		return nil, err
	}
	out.Records = records
	return out, nil
}

// internalPaths are the paths Waypoint creates inside a session's
// filesystem: the staged helper and completion FIFO under /.waypoint, the
// old root left by pivot_root, and the session directory, where the shell's
// control socket lives at the same path as on the host.
func internalPaths(ms ...*Manager) []string {
	out := []string{"/.waypoint", "/.waypoint-old-root"}
	seen := map[string]bool{}
	for _, m := range ms {
		if !seen[m.baseDir] {
			seen[m.baseDir] = true
			out = append(out, filepath.Clean(m.baseDir))
		}
	}
	return out
}

// isSessionDirAncestor reports whether p is a proper ancestor directory of
// the session directory; Waypoint creates those when it binds the socket.
func isSessionDirAncestor(p, sessionDir string) bool {
	sessionDir = filepath.Clean(sessionDir)
	return p != "/" && strings.HasPrefix(sessionDir, strings.TrimSuffix(p, "/")+"/")
}
