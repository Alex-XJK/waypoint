package waypoint

// Exact line matching for line-level blame.
//
// go-udiff's line diff bounds its search depth, so for files with many edits it can
// report unchanged lines as deleted and re-inserted. Blame would then date those
// lines to the later checkpoint. matchLines computes exact longest common
// subsequences instead (Myers' O(ND) algorithm with the linear-space middle-snake
// bisection, as in diff-match-patch), and falls back to go-udiff only past a work
// budget.
//
// A plain longest common subsequence counts a blank line as much as a line of code,
// so in a rewritten file it can keep scattered blank lines instead of a real line
// that survived (we saw it trade `def attack(...)` for two blank lines). matchLines
// therefore first keeps as many non-blank lines as possible, then matches the
// remaining lines inside each gap between them.

import (
	"strings"

	"github.com/aymanbagabas/go-udiff/lcs"
)

// lineMatchBudget bounds the diagonal steps of one exact match, about 10 s of
// work; past it, matchLines falls back to go-udiff's bounded diff and reports
// the result as approximate. A 32,000-line file with a quarter of its lines
// changed needs about 1.5e8. Variable so tests can lower it.
var lineMatchBudget = 1_000_000_000

// splitLines splits file content into lines that keep their "\n" terminators, so
// that adding or removing the final newline changes the last line, as in git. A
// trailing newline does not add an empty line.
func splitLines(content string) []string {
	if content == "" {
		return nil
	}
	lines := strings.SplitAfter(content, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// matchLines returns, for each line of after, the index of the line of before it
// is kept from, or -1 if it was inserted. The kept lines are in order. When exact
// is true, the kept non-blank lines are a longest common subsequence of the
// non-blank lines, and within each gap between two of them the kept lines are a
// longest common subsequence of that gap. When the work budget runs out, the
// whole match comes from go-udiff's bounded diff and exact is false.
func matchLines(before, after []string) (match []int, exact bool) {
	budget := lineMatchBudget
	match = make([]int, len(after))
	for i := range match {
		match[i] = -1
	}

	// 1. anchor on the non-blank lines
	sigA, sigB := nonBlankIndexes(before), nonBlankIndexes(after)
	sigMatch, ok := matchSeq(pick(before, sigA), pick(after, sigB), &budget)
	if !ok {
		return boundedMatch(before, after), false
	}
	for j, i := range sigMatch {
		if i >= 0 {
			match[sigB[j]] = sigA[i]
		}
	}

	// 2. match all lines inside each gap between consecutive anchors
	aLo, bLo := 0, 0
	fill := func(aHi, bHi int) bool {
		gap, ok := matchSeq(before[aLo:aHi], after[bLo:bHi], &budget)
		if !ok {
			return false
		}
		for j, i := range gap {
			if i >= 0 {
				match[bLo+j] = aLo + i
			}
		}
		return true
	}
	for j := 0; j < len(after); j++ {
		if match[j] < 0 {
			continue
		}
		if !fill(match[j], j) {
			return boundedMatch(before, after), false
		}
		aLo, bLo = match[j]+1, j+1
	}
	if !fill(len(before), len(after)) {
		return boundedMatch(before, after), false
	}
	return match, true
}

func nonBlankIndexes(lines []string) []int {
	var idx []int
	for i, line := range lines {
		if strings.TrimSpace(line) != "" {
			idx = append(idx, i)
		}
	}
	return idx
}

func pick(lines []string, idx []int) []string {
	out := make([]string, len(idx))
	for k, i := range idx {
		out[k] = lines[i]
	}
	return out
}

// matchSeq matches before against after by an exact longest common subsequence,
// charging budget. It returns false when the budget runs out.
func matchSeq(before, after []string, budget *int) ([]int, bool) {
	ids := make(map[string]int, len(before)+len(after))
	intern := func(lines []string) []int {
		out := make([]int, len(lines))
		for i, line := range lines {
			id, ok := ids[line]
			if !ok {
				id = len(ids)
				ids[line] = id
			}
			out[i] = id
		}
		return out
	}
	m := &lineMatcher{
		a:      intern(before),
		b:      intern(after),
		match:  make([]int, len(after)),
		budget: *budget,
	}
	for i := range m.match {
		m.match[i] = -1
	}
	ok := m.diff(0, len(m.a), 0, len(m.b))
	*budget = m.budget
	return m.match, ok
}

// boundedMatch converts go-udiff's (bounded) line diff into a match array.
func boundedMatch(before, after []string) []int {
	match := make([]int, len(after))
	for i := range match {
		match[i] = -1
	}
	ai, bi := 0, 0
	for _, d := range lcs.DiffLines(before, after) {
		for ; ai < d.Start && bi < d.ReplStart; ai, bi = ai+1, bi+1 {
			match[bi] = ai
		}
		ai, bi = d.End, d.ReplEnd
	}
	for ; ai < len(before) && bi < len(after); ai, bi = ai+1, bi+1 {
		match[bi] = ai
	}
	return match
}

type lineMatcher struct {
	a, b   []int
	match  []int
	budget int
}

// diff matches a[aLo:aHi] against b[bLo:bHi]. It returns false if the budget ran out.
func (m *lineMatcher) diff(aLo, aHi, bLo, bHi int) bool {
	for aLo < aHi && bLo < bHi && m.a[aLo] == m.b[bLo] {
		m.match[bLo] = aLo
		aLo, bLo = aLo+1, bLo+1
	}
	for aLo < aHi && bLo < bHi && m.a[aHi-1] == m.b[bHi-1] {
		aHi, bHi = aHi-1, bHi-1
		m.match[bHi] = aHi
	}
	if aLo == aHi || bLo == bHi {
		return true
	}
	// One line left on a side: it is kept if the other side has it anywhere.
	// bisect needs at least two lines on each side.
	if aHi-aLo == 1 {
		for j := bLo; j < bHi; j++ {
			if m.b[j] == m.a[aLo] {
				m.match[j] = aLo
				break
			}
		}
		return true
	}
	if bHi-bLo == 1 {
		for i := aLo; i < aHi; i++ {
			if m.a[i] == m.b[bLo] {
				m.match[bLo] = i
				break
			}
		}
		return true
	}
	x, y, found, ok := m.bisect(aLo, aHi, bLo, bHi)
	if !ok {
		return false
	}
	if !found {
		return true // nothing in common
	}
	return m.diff(aLo, x, bLo, y) && m.diff(x, aHi, y, bHi)
}

// bisect finds a point (x, y) on a shortest edit path of a[aLo:aHi] to
// b[bLo:bHi] by running the forward and reverse searches until they overlap.
// found is false when the ranges have nothing in common.
func (m *lineMatcher) bisect(aLo, aHi, bLo, bHi int) (x, y int, found, ok bool) {
	n, mm := aHi-aLo, bHi-bLo
	maxD := (n + mm + 1) / 2
	vOffset := maxD
	vLength := 2 * maxD
	v1 := make([]int, vLength)
	v2 := make([]int, vLength)
	for i := range v1 {
		v1[i], v2[i] = -1, -1
	}
	v1[vOffset+1], v2[vOffset+1] = 0, 0
	delta := n - mm
	// With an odd delta the forward path detects the overlap, else the reverse one.
	front := delta%2 != 0
	k1start, k1end, k2start, k2end := 0, 0, 0, 0
	for d := 0; d < maxD; d++ {
		m.budget -= 2*d + 2
		if m.budget < 0 {
			return 0, 0, false, false
		}
		for k1 := -d + k1start; k1 <= d-k1end; k1 += 2 {
			k1Offset := vOffset + k1
			var x1 int
			if k1 == -d || (k1 != d && v1[k1Offset-1] < v1[k1Offset+1]) {
				x1 = v1[k1Offset+1]
			} else {
				x1 = v1[k1Offset-1] + 1
			}
			y1 := x1 - k1
			for x1 < n && y1 < mm && m.a[aLo+x1] == m.b[bLo+y1] {
				x1, y1 = x1+1, y1+1
			}
			v1[k1Offset] = x1
			switch {
			case x1 > n:
				k1end += 2 // ran off the right of the graph
			case y1 > mm:
				k1start += 2 // ran off the bottom of the graph
			case front:
				k2Offset := vOffset + delta - k1
				if k2Offset >= 0 && k2Offset < vLength && v2[k2Offset] != -1 {
					if x1 >= n-v2[k2Offset] { // mirror x2 onto the forward coordinates
						return aLo + x1, bLo + y1, true, true
					}
				}
			}
		}
		for k2 := -d + k2start; k2 <= d-k2end; k2 += 2 {
			k2Offset := vOffset + k2
			var x2 int
			if k2 == -d || (k2 != d && v2[k2Offset-1] < v2[k2Offset+1]) {
				x2 = v2[k2Offset+1]
			} else {
				x2 = v2[k2Offset-1] + 1
			}
			y2 := x2 - k2
			for x2 < n && y2 < mm && m.a[aLo+n-x2-1] == m.b[bLo+mm-y2-1] {
				x2, y2 = x2+1, y2+1
			}
			v2[k2Offset] = x2
			switch {
			case x2 > n:
				k2end += 2
			case y2 > mm:
				k2start += 2
			case !front:
				k1Offset := vOffset + delta - k2
				if k1Offset >= 0 && k1Offset < vLength && v1[k1Offset] != -1 {
					x1 := v1[k1Offset]
					y1 := vOffset + x1 - k1Offset
					if x1 >= n-x2 {
						return aLo + x1, bLo + y1, true, true
					}
				}
			}
		}
	}
	return 0, 0, false, true
}
