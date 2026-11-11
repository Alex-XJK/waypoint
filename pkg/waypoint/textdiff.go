package waypoint

// Line diffs of text files for `waypoint diff --content`, in the unified
// format of `diff -u` and `git diff`, so a patch can be read by a person or
// a model, or applied with `patch -p1` / `git apply`. With --word-diff the
// hunks are the same, but each run of changed lines is shown once, with the
// changed words marked inline as [-old-]{+new+}, as `git diff
// --word-diff=plain` does; the words are aligned by a second diff over
// tokens (runs of non-space, runs of space). A file whose paragraphs are
// single lines, or a long minified line, then shows which words changed
// instead of a whole line removed and re-added. A word patch is for reading:
// patch(1) cannot apply it.
//
// The edit script comes from Myers' O(ND) algorithm on the lines left after
// the common prefix and suffix are trimmed. Lines keep their terminating
// newline, so a missing final newline is a difference like any other and is
// marked with "\ No newline at end of file". The search stops after
// maxEditDistance edits; past that, the untrimmed middle is reported as
// deleted and re-added, which is still a correct patch, only not a minimal
// one.

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

// DefaultPatchMaxBytes is the largest file `waypoint diff --content` diffs.
const DefaultPatchMaxBytes = 1 << 20

const (
	patchContext    = 3
	maxEditDistance = 1000
)

// Reasons a file record carries no patch.
const (
	patchOmittedTooLarge = "too_large"
	patchOmittedBinary   = "binary"
	patchOmittedNotUTF8  = "not_utf8"
)

// wantsPatch reports whether a record is about file content: one side is a
// regular file, and the file was added, deleted, or its content or type
// changed.
func wantsPatch(rec *FileRecord) bool {
	isFile := func(e *FileEntry) bool { return e != nil && e.Type == "file" }
	if !isFile(rec.From) && !isFile(rec.To) {
		return false
	}
	if rec.Change != "modified" {
		return true
	}
	for _, f := range rec.Fields {
		if f == "content" || f == "type" {
			return true
		}
	}
	return false
}

// patchOptions selects line diffs of file content; MaxBytes 0 means none.
type patchOptions struct {
	MaxBytes int64
	Words    bool // inline word marks instead of -/+ lines
}

// filePatch diffs the regular files at fromHost and toHost; an empty host
// path stands for a side that has no file there. It returns either a patch
// or the reason it omitted one.
func filePatch(path, fromHost string, fromSize int64, toHost string, toSize int64, opts patchOptions) (patch, omitted string, err error) {
	max := opts.MaxBytes
	if fromSize > max || toSize > max {
		return "", patchOmittedTooLarge, nil
	}
	read := func(host string) ([]byte, error) {
		if host == "" {
			return nil, nil
		}
		return os.ReadFile(host)
	}
	a, err := read(fromHost)
	if err != nil {
		return "", "", err
	}
	b, err := read(toHost)
	if err != nil {
		return "", "", err
	}
	// Git's test: a NUL byte means binary.
	if bytes.IndexByte(a, 0) >= 0 || bytes.IndexByte(b, 0) >= 0 {
		return "", patchOmittedBinary, nil
	}
	// JSON strings hold UTF-8 only; other encodings would come out mangled.
	if !utf8.Valid(a) || !utf8.Valid(b) {
		return "", patchOmittedNotUTF8, nil
	}
	fromName, toName := "a"+path, "b"+path
	if fromHost == "" {
		fromName = "/dev/null"
	}
	if toHost == "" {
		toName = "/dev/null"
	}
	return textPatch(fromName, toName, string(a), string(b), opts.Words), "", nil
}

// splitLines splits text into lines that keep their "\n".
func splitLines(s string) []string {
	var out []string
	for s != "" {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			out = append(out, s)
			break
		}
		out = append(out, s[:i+1])
		s = s[i+1:]
	}
	return out
}

type lineOp struct {
	kind byte // ' ', '-', '+'
	line string
}

// diffLines returns an edit script turning a into b; the elements are
// lines, or tokens for the word level.
func diffLines(a, b []string) []lineOp {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	ops := make([]lineOp, 0, len(a)+len(b)-pre-suf)
	for _, l := range a[:pre] {
		ops = append(ops, lineOp{' ', l})
	}
	ma, mb := a[pre:len(a)-suf], b[pre:len(b)-suf]
	if mid, ok := myers(ma, mb, maxEditDistance); ok {
		ops = append(ops, mid...)
	} else {
		for _, l := range ma {
			ops = append(ops, lineOp{'-', l})
		}
		for _, l := range mb {
			ops = append(ops, lineOp{'+', l})
		}
	}
	for _, l := range a[len(a)-suf:] {
		ops = append(ops, lineOp{' ', l})
	}
	return ops
}

// myers finds a shortest edit script of at most limit edits.
func myers(a, b []string, limit int) ([]lineOp, bool) {
	// Compare small integers instead of strings.
	ids := map[string]int{}
	intern := func(ls []string) []int {
		out := make([]int, len(ls))
		for i, l := range ls {
			id, ok := ids[l]
			if !ok {
				id = len(ids)
				ids[l] = id
			}
			out[i] = id
		}
		return out
	}
	x, y := intern(a), intern(b)
	n, m := len(x), len(y)
	if n+m < limit {
		limit = n + m
	}
	// v[k+off] is the furthest x on diagonal k. trace[d] keeps v as it was
	// before step d, for diagonals -(d+1)..d+1.
	off := limit + 1
	v := make([]int, 2*off+1)
	var trace [][]int
	found := false
	for d := 0; d <= limit && !found; d++ {
		trace = append(trace, append([]int(nil), v[off-d-1:off+d+2]...))
		for k := -d; k <= d; k += 2 {
			var px int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				px = v[off+k+1] // down: insert
			} else {
				px = v[off+k-1] + 1 // right: delete
			}
			py := px - k
			for px < n && py < m && x[px] == y[py] {
				px++
				py++
			}
			v[off+k] = px
			if px >= n && py >= m {
				found = true
				break
			}
		}
	}
	if !found {
		return nil, false
	}
	// Walk back from (n, m), collecting the script in reverse.
	var rev []lineOp
	cx, cy := n, m
	for d := len(trace) - 1; d >= 0; d-- {
		tv := trace[d]
		at := func(k int) int { return tv[k+d+1] }
		k := cx - cy
		var pk int
		if k == -d || (k != d && at(k-1) < at(k+1)) {
			pk = k + 1
		} else {
			pk = k - 1
		}
		px := at(pk)
		py := px - pk
		if d == 0 {
			px, py = 0, 0
		}
		for cx > px && cy > py {
			cx--
			cy--
			rev = append(rev, lineOp{' ', a[cx]})
		}
		if d > 0 {
			if cx == px {
				cy--
				rev = append(rev, lineOp{'+', b[cy]})
			} else {
				cx--
				rev = append(rev, lineOp{'-', a[cx]})
			}
		}
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev, true
}

// unifiedDiff formats the line diff of a and b; it is empty when they are
// equal.
func unifiedDiff(fromName, toName, a, b string) string {
	return textPatch(fromName, toName, a, b, false)
}

// textPatch formats the diff of a and b with -/+ lines, or with inline word
// marks when words is set.
func textPatch(fromName, toName, a, b string, words bool) string {
	ops := diffLines(splitLines(a), splitLines(b))
	// aPos[i], bPos[i]: lines of a and b before ops[i].
	aPos, bPos := make([]int, len(ops)+1), make([]int, len(ops)+1)
	var changes []int
	for i, op := range ops {
		aPos[i+1], bPos[i+1] = aPos[i], bPos[i]
		if op.kind != '+' {
			aPos[i+1]++
		}
		if op.kind != '-' {
			bPos[i+1]++
		}
		if op.kind != ' ' {
			changes = append(changes, i)
		}
	}
	if len(changes) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", fromName, toName)
	for i := 0; i < len(changes); {
		// A hunk runs from patchContext lines before a change to
		// patchContext lines after the last change that is no more than
		// 2*patchContext equal lines from the previous one.
		j := i
		for j+1 < len(changes) && changes[j+1]-changes[j] <= 2*patchContext+1 {
			j++
		}
		start := max(changes[i]-patchContext, 0)
		end := min(changes[j]+patchContext+1, len(ops))
		fmt.Fprintf(&sb, "@@ -%s +%s @@\n", hunkRange(aPos[start], aPos[end]-aPos[start]), hunkRange(bPos[start], bPos[end]-bPos[start]))
		if words {
			writeWordHunk(&sb, ops[start:end])
		} else {
			for _, op := range ops[start:end] {
				sb.WriteByte(op.kind)
				sb.WriteString(op.line)
				if !strings.HasSuffix(op.line, "\n") {
					sb.WriteString("\n\\ No newline at end of file\n")
				}
			}
		}
		i = j + 1
	}
	return sb.String()
}

// hunkRange formats "start,count" the way diff -u does: a one-line range is
// just its line number, and an empty range names the line before it.
func hunkRange(before, count int) string {
	switch count {
	case 0:
		return fmt.Sprintf("%d,0", before)
	case 1:
		return fmt.Sprintf("%d", before+1)
	}
	return fmt.Sprintf("%d,%d", before+1, count)
}

// writeWordHunk writes a hunk's lines without -/+ prefixes: equal lines as
// they are, and each run of removed and added lines as one text in which
// the differing tokens are marked [-removed-] and {+added+}.
func writeWordHunk(sb *strings.Builder, ops []lineOp) {
	for k := 0; k < len(ops); {
		if ops[k].kind == ' ' {
			sb.WriteString(ops[k].line)
			k++
			continue
		}
		var del, ins strings.Builder
		for ; k < len(ops) && ops[k].kind != ' '; k++ {
			if ops[k].kind == '-' {
				del.WriteString(ops[k].line)
			} else {
				ins.WriteString(ops[k].line)
			}
		}
		writeWordRun(sb, del.String(), ins.String())
	}
	// The text ends without a newline (one side lacks it, as the marks
	// show): end the line for display and say so, as diff -u does.
	if s := sb.String(); !strings.HasSuffix(s, "\n") {
		sb.WriteString("\n\\ No newline at end of file\n")
	}
}

// writeWordRun marks the token-level differences between a and b.
func writeWordRun(sb *strings.Builder, a, b string) {
	ops := diffLines(splitTokens(a), splitTokens(b))
	var del, ins strings.Builder
	flush := func() {
		if del.Len() > 0 {
			sb.WriteString("[-" + del.String() + "-]")
			del.Reset()
		}
		if ins.Len() > 0 {
			sb.WriteString("{+" + ins.String() + "+}")
			ins.Reset()
		}
	}
	for _, op := range ops {
		switch op.kind {
		case ' ':
			flush()
			sb.WriteString(op.line)
		case '-':
			del.WriteString(op.line)
		case '+':
			ins.WriteString(op.line)
		}
	}
	flush()
}

// splitTokens splits text into alternating runs of non-space and space.
func splitTokens(s string) []string {
	var out []string
	for s != "" {
		space := isSpace(s[0])
		i := 1
		for i < len(s) && isSpace(s[i]) == space {
			i++
		}
		out = append(out, s[:i])
		s = s[i:]
	}
	return out
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}
