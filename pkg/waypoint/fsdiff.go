package waypoint

// Filesystem diff between two checkpoints, computed from their layers
// without mounting them.
//
// A checkpoint's filesystem is a stack of layers: the sealed upper directory
// of each checkpoint in LayerIDs, newest on top, over the session's original
// rootfs. A path is looked up the way OverlayFS looks it up (with
// redirect_dir and metacopy off, as Waypoint mounts it): component by
// component, the topmost entry decides; a whiteout hides the name in lower
// layers; a non-directory or an opaque directory hides everything below it.
//
// Only some paths can differ. The two stacks share a bottom part: the
// layers of their common ancestor within a session, or the original rootfs
// alone. A path can resolve differently only if a layer above the shared
// part has an entry at the path, or has a whiteout, an opaque directory or a
// non-directory at the path that hides what is below it. The candidates are
// therefore the entries of the unshared layers, plus, under each such hiding
// entry, everything either side sees there. When the two sides do not share
// an original rootfs (two sessions), the paths where the two originals
// differ are candidates too; they come from a manifest of each original,
// computed once per session. Every candidate is resolved on both sides and
// compared. The cost follows what the unshared layers wrote, not the size of
// the rootfs.
//
// Compared: type, permission bits, owner, content (SHA-256), symlink target
// and device numbers. Not compared: timestamps, extended attributes and hard
// link identity. Paths Waypoint itself creates (/.waypoint, the session
// directory that holds the shell control socket) are left out.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// FilesystemDiff is the filesystem part of `waypoint diff`.
type FilesystemDiff struct {
	Base FSBase `json:"base"`
	// FromLayers and ToLayers are the layers above the shared part, oldest
	// first; their entries are the candidates.
	FromLayers []string `json:"from_layers"`
	ToLayers   []string `json:"to_layers"`
	Excluded   []string `json:"excluded"`
	// PatchFormat is "unified" or "word" when records carry patches.
	PatchFormat string       `json:"patch_format,omitempty"`
	Records     []FileRecord `json:"records"`
}

// FSBase says what the two stacks share.
type FSBase struct {
	// shared_layers: same session; the original and SharedLayers are common.
	// equal_originals: two originals with the same manifest.
	// different_originals: their differences are records with base_only.
	Kind               string   `json:"kind"`
	SharedLayers       []string `json:"shared_layers,omitempty"`
	FromOriginalDigest string   `json:"from_original_digest,omitempty"`
	ToOriginalDigest   string   `json:"to_original_digest,omitempty"`
}

type FileRecord struct {
	Path   string `json:"path"`
	Change string `json:"change"` // added, deleted, modified
	// Fields lists what differs for a modified path: type, mode, uid, gid,
	// content, target, rdev.
	Fields []string `json:"fields,omitempty"`
	// BaseOnly: both sides resolve the path from their original rootfs.
	BaseOnly bool       `json:"base_only,omitempty"`
	From     *FileEntry `json:"from"`
	To       *FileEntry `json:"to"`
	// Patch is the unified line diff of a regular file's content (with
	// --content); PatchOmitted says why there is none: too_large, binary or
	// not_utf8.
	Patch        string `json:"patch,omitempty"`
	PatchOmitted string `json:"patch_omitted,omitempty"`
}

type FileEntry struct {
	Type   string `json:"type"` // file, dir, symlink, char, block, fifo, socket
	Mode   string `json:"mode"` // permission, setuid, setgid and sticky bits, octal
	UID    uint32 `json:"uid"`
	GID    uint32 `json:"gid"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Target string `json:"target,omitempty"`
	Rdev   string `json:"rdev,omitempty"`
	Layer  string `json:"layer"` // checkpoint whose upper holds the entry, or "original"
}

const originalLayerID = "original"

type fsLayer struct {
	id  string
	dir string
}

// fsView is one checkpoint's layer stack, top first, original last.
type fsView struct {
	layers []fsLayer
	hashes map[string]string // host path -> sha256, per diff
}

type entryKind int

const (
	entryNone entryKind = iota
	entryWhiteout
	entryDir
	entryOpaqueDir
	entryOther
)

// classify looks at one layer entry.
func classify(hostPath string, upper bool) (entryKind, *syscall.Stat_t, error) {
	var st syscall.Stat_t
	if err := syscall.Lstat(hostPath, &st); err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) {
			return entryNone, nil, nil
		}
		return entryNone, nil, fmt.Errorf("lstat %s: %w", hostPath, err)
	}
	switch st.Mode & syscall.S_IFMT {
	case syscall.S_IFCHR:
		if upper && st.Rdev == 0 {
			return entryWhiteout, &st, nil
		}
	case syscall.S_IFREG:
		if upper && st.Size == 0 && hasXattr(hostPath, "trusted.overlay.whiteout") {
			return entryWhiteout, &st, nil
		}
		if upper && hasXattr(hostPath, "trusted.overlay.metacopy") {
			return entryNone, nil, fmt.Errorf("%s is a metacopy entry, which diff does not support", hostPath)
		}
	case syscall.S_IFDIR:
		if !upper {
			return entryDir, &st, nil
		}
		if hasXattr(hostPath, "trusted.overlay.redirect") {
			return entryNone, nil, fmt.Errorf("%s is a redirected directory, which diff does not support", hostPath)
		}
		var v [8]byte
		if n, err := unix.Lgetxattr(hostPath, "trusted.overlay.opaque", v[:]); err == nil && string(v[:n]) == "y" {
			return entryOpaqueDir, &st, nil
		}
		return entryDir, &st, nil
	}
	return entryOther, &st, nil
}

func hasXattr(path, name string) bool {
	_, err := unix.Lgetxattr(path, name, nil)
	return err == nil
}

// resolved is the entry a stack shows at one path.
type resolved struct {
	layer int // index into fsView.layers
	host  string
	st    *syscall.Stat_t
	// dirLayers are the layers whose directories merge into this one, top
	// first (directories only).
	dirLayers []int
}

func (r *resolved) isDir() bool { return r != nil && r.st.Mode&syscall.S_IFMT == syscall.S_IFDIR }

// step resolves name inside a directory merged from dirLayers.
func (v *fsView) step(dirLayers []int, p string) (*resolved, error) {
	var top *resolved
	for _, li := range dirLayers {
		l := v.layers[li]
		host := filepath.Join(l.dir, p)
		kind, st, err := classify(host, l.id != originalLayerID)
		if err != nil {
			return nil, err
		}
		stop := false
		switch kind {
		case entryNone:
			continue
		case entryWhiteout:
			stop = true
		case entryDir, entryOpaqueDir:
			if top == nil {
				top = &resolved{layer: li, host: host, st: st}
			}
			top.dirLayers = append(top.dirLayers, li)
			stop = kind == entryOpaqueDir
		case entryOther:
			if top == nil {
				top = &resolved{layer: li, host: host, st: st}
			}
			stop = true
		}
		if stop {
			break
		}
	}
	return top, nil
}

func (v *fsView) allLayers() []int {
	out := make([]int, len(v.layers))
	for i := range out {
		out[i] = i
	}
	return out
}

// resolve looks up an absolute path; nil means it does not exist.
func (v *fsView) resolve(p string) (*resolved, error) {
	cur := &resolved{dirLayers: v.allLayers()}
	prefix := "/"
	for _, c := range strings.Split(strings.Trim(p, "/"), "/") {
		if c == "" {
			continue
		}
		prefix = filepath.Join(prefix, c)
		next, err := v.step(cur.dirLayers, prefix)
		if err != nil || next == nil {
			return nil, err
		}
		if prefix != p && !next.isDir() {
			return nil, nil // a non-directory hides the rest of the path
		}
		cur = next
	}
	if p == "/" {
		return v.step(cur.dirLayers, "/")
	}
	return cur, nil
}

// readDir lists the names a merged directory shows.
func (v *fsView) readDir(dir *resolved, p string) ([]string, error) {
	seen := map[string]bool{}
	var names []string
	for _, li := range dir.dirLayers {
		l := v.layers[li]
		entries, err := os.ReadDir(filepath.Join(l.dir, p))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, e := range entries {
			if seen[e.Name()] {
				continue
			}
			seen[e.Name()] = true
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// walk calls fn for every path the stack shows below p (not p itself).
func (v *fsView) walk(p string, dir *resolved, skip func(string) bool, fn func(string)) error {
	names, err := v.readDir(dir, p)
	if err != nil {
		return err
	}
	for _, name := range names {
		child := filepath.Join(p, name)
		if skip(child) {
			continue
		}
		r, err := v.step(dir.dirLayers, child)
		if err != nil {
			return err
		}
		if r == nil {
			continue // whiteout
		}
		fn(child)
		if r.isDir() {
			if err := v.walk(child, r, skip, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// entry describes a resolved path for the output.
func (v *fsView) entry(r *resolved) (*FileEntry, error) {
	if r == nil {
		return nil, nil
	}
	st := r.st
	e := &FileEntry{
		Mode:  fmt.Sprintf("%04o", st.Mode&0o7777),
		UID:   st.Uid,
		GID:   st.Gid,
		Layer: v.layers[r.layer].id,
	}
	switch st.Mode & syscall.S_IFMT {
	case syscall.S_IFREG:
		e.Type, e.Size = "file", st.Size
		sum, err := v.sha256(r.host)
		if err != nil {
			return nil, err
		}
		e.SHA256 = sum
	case syscall.S_IFDIR:
		e.Type = "dir"
	case syscall.S_IFLNK:
		e.Type = "symlink"
		t, err := os.Readlink(r.host)
		if err != nil {
			return nil, err
		}
		e.Target = t
	case syscall.S_IFCHR, syscall.S_IFBLK:
		e.Type = "char"
		if st.Mode&syscall.S_IFMT == syscall.S_IFBLK {
			e.Type = "block"
		}
		e.Rdev = fmt.Sprintf("%d:%d", unix.Major(uint64(st.Rdev)), unix.Minor(uint64(st.Rdev)))
	case syscall.S_IFIFO:
		e.Type = "fifo"
	case syscall.S_IFSOCK:
		e.Type = "socket"
	default:
		e.Type = "other"
	}
	return e, nil
}

func (v *fsView) sha256(host string) (string, error) {
	if sum, ok := v.hashes[host]; ok {
		return sum, nil
	}
	sum, err := fileSHA256(host)
	if err != nil {
		return "", err
	}
	v.hashes[host] = sum
	return sum, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// changedFields compares two entries of the same path.
func changedFields(a, b *FileEntry) []string {
	var out []string
	if a.Type != b.Type {
		return []string{"type"}
	}
	if a.Mode != b.Mode {
		out = append(out, "mode")
	}
	if a.UID != b.UID {
		out = append(out, "uid")
	}
	if a.GID != b.GID {
		out = append(out, "gid")
	}
	if a.SHA256 != b.SHA256 {
		out = append(out, "content")
	}
	if a.Target != b.Target {
		out = append(out, "target")
	}
	if a.Rdev != b.Rdev {
		out = append(out, "rdev")
	}
	return out
}

// collectLayer adds every path in an upper layer to cands, and marks the
// entries that hide lower layers (whiteouts, opaque dirs, non-dirs).
func collectLayer(dir string, skip func(string) bool, cands, hiding map[string]bool) error {
	return filepath.WalkDir(dir, func(host string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, host)
		if rel == "." {
			return nil
		}
		p := "/" + rel
		if skip(p) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		cands[p] = true
		kind, _, err := classify(host, true)
		if err != nil {
			return err
		}
		if kind != entryDir {
			hiding[p] = true
		}
		return nil
	})
}

// diffFilesystems compares two stacks. shared is the number of bottom
// layers (counting the original) the two have in common; baseDiffs are the
// paths where two unshared originals differ. skip leaves out a path and
// everything below it; drop leaves out the record of the path alone.
// popts.MaxBytes > 0 adds line diffs of text files up to that size.
func diffFilesystems(from, to *fsView, shared int, baseDiffs []string, skip, drop func(string) bool, popts patchOptions) ([]FileRecord, error) {
	cands, hiding := map[string]bool{}, map[string]bool{}
	for _, v := range []*fsView{from, to} {
		for _, l := range v.layers[:len(v.layers)-shared] {
			if l.id == originalLayerID {
				continue
			}
			if err := collectLayer(l.dir, skip, cands, hiding); err != nil {
				return nil, err
			}
		}
	}
	for _, p := range baseDiffs {
		if !skip(p) {
			cands[p] = true
		}
	}
	// Under an entry that hides lower layers, everything either side shows
	// is a candidate: the side that kept the old content has paths that no
	// unshared layer names.
	for _, p := range sortedKeys(hiding) {
		for _, v := range []*fsView{from, to} {
			r, err := v.resolve(p)
			if err != nil {
				return nil, err
			}
			if r.isDir() {
				if err := v.walk(p, r, skip, func(c string) { cands[c] = true }); err != nil {
					return nil, err
				}
			}
		}
	}

	records := []FileRecord{}
	for _, p := range sortedKeys(cands) {
		if drop(p) {
			continue
		}
		ra, err := from.resolve(p)
		if err != nil {
			return nil, err
		}
		rb, err := to.resolve(p)
		if err != nil {
			return nil, err
		}
		if ra == nil && rb == nil {
			continue
		}
		// The same host file on both sides (a shared layer) is equal
		// without reading it.
		if ra != nil && rb != nil && ra.host == rb.host {
			continue
		}
		ea, err := from.entry(ra)
		if err != nil {
			return nil, err
		}
		eb, err := to.entry(rb)
		if err != nil {
			return nil, err
		}
		rec := FileRecord{Path: p, From: ea, To: eb}
		switch {
		case ea == nil:
			rec.Change = "added"
		case eb == nil:
			rec.Change = "deleted"
		default:
			rec.Fields = changedFields(ea, eb)
			if len(rec.Fields) == 0 {
				continue
			}
			rec.Change = "modified"
		}
		rec.BaseOnly = (ea == nil || ea.Layer == originalLayerID) && (eb == nil || eb.Layer == originalLayerID)
		if popts.MaxBytes > 0 && wantsPatch(&rec) {
			fileSide := func(e *FileEntry, r *resolved) (string, int64) {
				if e == nil || e.Type != "file" {
					return "", 0
				}
				return r.host, e.Size
			}
			ah, as := fileSide(ea, ra)
			bh, bs := fileSide(eb, rb)
			if rec.Patch, rec.PatchOmitted, err = filePatch(p, ah, as, bh, bs, popts); err != nil {
				return nil, err
			}
		}
		records = append(records, rec)
	}
	return records, nil
}

// --- original rootfs manifests (diffs across sessions) ---

const originalManifestName = "original.manifest"

// originalManifest returns the manifest of a session's original rootfs:
// one sorted line per path with its type, mode, owner, size, content hash,
// symlink target and device numbers. Originals are immutable once staged, so
// it is computed once and kept in the session directory.
func (m *Manager) originalManifest() (path, digest string, err error) {
	path = filepath.Join(m.baseDir, originalManifestName)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		data, err := buildManifest(m.originalDir)
		if err != nil {
			return "", "", fmt.Errorf("manifest of %s: %w", m.originalDir, err)
		}
		if err := atomicWriteFile(path, data, 0o644); err != nil {
			return "", "", err
		}
	}
	sum, err := fileSHA256(path)
	return path, sum, err
}

func buildManifest(root string) ([]byte, error) {
	v := &fsView{layers: []fsLayer{{id: originalLayerID, dir: root}}, hashes: map[string]string{}}
	var lines []string
	err := filepath.WalkDir(root, func(host string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, host)
		if rel == "." {
			return nil
		}
		var st syscall.Stat_t
		if err := syscall.Lstat(host, &st); err != nil {
			return err
		}
		e, err := v.entry(&resolved{host: host, st: &st})
		if err != nil {
			return err
		}
		lines = append(lines, manifestLine("/"+rel, e))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(lines)
	return []byte(strings.Join(lines, "")), nil
}

func manifestLine(p string, e *FileEntry) string {
	// Paths are quoted so that tabs and newlines in names cannot break the format.
	return fmt.Sprintf("%q\t%s\t%s\t%d\t%d\t%d\t%s\t%q\t%s\n", p, e.Type, e.Mode, e.UID, e.GID, e.Size, e.SHA256, e.Target, e.Rdev)
}

// manifestDiffs returns the paths whose manifest lines differ.
func manifestDiffs(pathA, pathB string) ([]string, error) {
	read := func(path string) (map[string]string, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		out := map[string]string{}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
		for sc.Scan() {
			line := sc.Bytes()
			i := bytes.IndexByte(line, '\t')
			if i < 0 {
				return nil, fmt.Errorf("manifest %s: malformed line", path)
			}
			var p string
			if _, err := fmt.Sscanf(string(line[:i]), "%q", &p); err != nil {
				return nil, fmt.Errorf("manifest %s: %w", path, err)
			}
			out[p] = string(line[i+1:])
		}
		return out, sc.Err()
	}
	a, err := read(pathA)
	if err != nil {
		return nil, err
	}
	b, err := read(pathB)
	if err != nil {
		return nil, err
	}
	var out []string
	for p, la := range a {
		if b[p] != la {
			out = append(out, p)
		}
	}
	for p := range b {
		if _, ok := a[p]; !ok {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}
