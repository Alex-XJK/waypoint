package waypoint

import (
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestUnifiedDiffFormat(t *testing.T) {
	cases := []struct {
		name, a, b, want string
	}{
		{"equal", "x\ny\n", "x\ny\n", ""},
		{"one line changed in the middle",
			"1\n2\n3\n4\n5\n6\n7\n8\n9\n", "1\n2\n3\n4\nFIVE\n6\n7\n8\n9\n",
			"--- a/f\n+++ b/f\n@@ -2,7 +2,7 @@\n 2\n 3\n 4\n-5\n+FIVE\n 6\n 7\n 8\n"},
		{"added file", "", "hello\nworld\n",
			"--- a/f\n+++ b/f\n@@ -0,0 +1,2 @@\n+hello\n+world\n"},
		{"single line hunk", "a\n", "b\n",
			"--- a/f\n+++ b/f\n@@ -1 +1 @@\n-a\n+b\n"},
		{"missing final newline", "a\nb\n", "a\nb",
			"--- a/f\n+++ b/f\n@@ -1,2 +1,2 @@\n a\n-b\n+b\n\\ No newline at end of file\n"},
		{"two hunks far apart",
			"1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n", "X\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\nY\n",
			"--- a/f\n+++ b/f\n@@ -1,4 +1,4 @@\n-1\n+X\n 2\n 3\n 4\n@@ -9,4 +9,4 @@\n 9\n 10\n 11\n-12\n+Y\n"},
		{"changes six equal lines apart share a hunk",
			"1\n2\n3\n4\n5\n6\n7\n8\n", "X\n2\n3\n4\n5\n6\n7\nY\n",
			"--- a/f\n+++ b/f\n@@ -1,8 +1,8 @@\n-1\n+X\n 2\n 3\n 4\n 5\n 6\n 7\n-8\n+Y\n"},
	}
	for _, c := range cases {
		if got := unifiedDiff("a/f", "b/f", c.a, c.b); got != c.want {
			t.Errorf("%s:\ngot:\n%s\nwant:\n%s", c.name, got, c.want)
		}
	}
}

// lcs is the reference length of the longest common subsequence.
func lcs(a, b []string) int {
	dp := make([][]int, len(a)+1)
	for i := range dp {
		dp[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	return dp[0][0]
}

// TestDiffLinesProperties: on random inputs, the script rebuilds both sides
// and is minimal; past the edit limit it is still correct.
func TestDiffLinesProperties(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	gen := func() []string {
		out := make([]string, rng.Intn(30))
		for i := range out {
			out[i] = string(rune('a'+rng.Intn(4))) + "\n"
		}
		return out
	}
	check := func(a, b []string, ops []lineOp) (edits int) {
		var ra, rb []string
		for _, op := range ops {
			if op.kind != '+' {
				ra = append(ra, op.line)
			}
			if op.kind != '-' {
				rb = append(rb, op.line)
			}
			if op.kind != ' ' {
				edits++
			}
		}
		if strings.Join(ra, "") != strings.Join(a, "") || strings.Join(rb, "") != strings.Join(b, "") {
			t.Fatalf("script does not rebuild the inputs:\na=%q\nb=%q\nops=%q", a, b, ops)
		}
		return edits
	}
	for i := 0; i < 2000; i++ {
		a, b := gen(), gen()
		edits := check(a, b, diffLines(a, b))
		if want := len(a) + len(b) - 2*lcs(a, b); edits != want {
			t.Fatalf("%d edits, minimum is %d:\na=%q\nb=%q", edits, want, a, b)
		}
		if ops, ok := myers(a, b, 3); ok {
			check(a, b, ops)
		}
	}
	// Past the limit diffLines falls back to delete-all, insert-all.
	var a, b []string
	for i := 0; i < 3*maxEditDistance; i++ {
		a = append(a, "a"+string(rune('0'+i%10))+"\n")
		b = append(b, "b"+string(rune('0'+i%10))+"\n")
	}
	if edits := check(a, b, diffLines(a, b)); edits != len(a)+len(b) {
		t.Fatalf("fallback made %d edits", edits)
	}
}

func TestFilePatchOmits(t *testing.T) {
	dir := t.TempDir()
	w := func(name, content string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(content), 0o644)
		return p
	}
	text, bin, latin1 := w("t", "x\n"), w("b", "x\x00y"), w("l", "caf\xe9\n")
	for _, c := range []struct {
		name     string
		from     string
		max      int64
		omitted  string
		hasPatch bool
	}{
		{"text", text, 100, "", true},
		{"binary", bin, 100, patchOmittedBinary, false},
		{"latin-1", latin1, 100, patchOmittedNotUTF8, false},
		{"too large", text, 1, patchOmittedTooLarge, false},
	} {
		st, _ := os.Stat(c.from)
		patch, omitted, err := filePatch("/f", c.from, st.Size(), "", 0, patchOptions{MaxBytes: c.max})
		if err != nil || omitted != c.omitted || (patch != "") != c.hasPatch {
			t.Errorf("%s: patch %q omitted %q err %v", c.name, patch, omitted, err)
		}
	}
	patch, _, _ := filePatch("/etc/f", text, 2, "", 0, patchOptions{MaxBytes: 100})
	if !strings.HasPrefix(patch, "--- a/etc/f\n+++ /dev/null\n@@ -1 +0,0 @@\n-x\n") {
		t.Errorf("deleted file patch = %q", patch)
	}
}

func TestDiffFilesystemsPatches(t *testing.T) {
	orig := map[string]string{"/a/f": "1\n2\n3\n", "/mode": "m\n"}
	from := layers(t, orig)
	to := layers(t, map[string]string{"/a/f": "1\ntwo\n3\n", "/new": "n\n"}, orig)
	to.layers[1].dir = from.layers[0].dir
	// A mode-only change carries no patch.
	os.WriteFile(filepath.Join(to.layers[0].dir, "mode"), []byte("m\n"), 0o600)
	recs, err := diffFilesystems(from, to, 1, nil, never, never, patchOptions{MaxBytes: DefaultPatchMaxBytes})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]FileRecord{}
	for _, r := range recs {
		got[r.Path] = r
	}
	if p := got["/a/f"].Patch; p != "--- a/a/f\n+++ b/a/f\n@@ -1,3 +1,3 @@\n 1\n-2\n+two\n 3\n" {
		t.Errorf("/a/f patch = %q", p)
	}
	if p := got["/new"].Patch; p != "--- /dev/null\n+++ b/new\n@@ -0,0 +1 @@\n+n\n" {
		t.Errorf("/new patch = %q", p)
	}
	if r := got["/mode"]; r.Change != "modified" || r.Patch != "" || r.PatchOmitted != "" {
		t.Errorf("/mode = %+v", r)
	}
	// Without patchMax, no record carries a patch.
	recs, _ = diffFilesystems(from, to, 1, nil, never, never, patchOptions{})
	for _, r := range recs {
		if r.Patch != "" {
			t.Errorf("%s has a patch without --content", r.Path)
		}
	}
}

func TestWordPatchFormat(t *testing.T) {
	a := "1\n2\nThe quick brown fox jumps over the lazy dog.\n4\n"
	b := "1\n2\nThe fast brown fox leaps over the idle dog.\n4\n"
	want := "--- a/f\n+++ b/f\n@@ -1,4 +1,4 @@\n1\n2\nThe [-quick-]{+fast+} brown fox [-jumps-]{+leaps+} over the [-lazy-]{+idle+} dog.\n4\n"
	if got := textPatch("a/f", "b/f", a, b, true); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	// Removed and added lines next to each other are diffed as one text.
	got := textPatch("a/f", "b/f", "x\nold line\ny\n", "x\nnew line\nextra\ny\n", true)
	if want := "--- a/f\n+++ b/f\n@@ -1,3 +1,4 @@\nx\n[-old-]{+new+} line{+\nextra+}\ny\n"; got != want {
		t.Errorf("run of lines:\ngot:\n%q\nwant:\n%q", got, want)
	}
	if got := textPatch("a/f", "b/f", "same\n", "same\n", true); got != "" {
		t.Errorf("equal texts gave %q", got)
	}
}

// unmark rebuilds one side of a word patch body: the old side keeps [-...-]
// and drops {+...+}, the new side the opposite.
func unmark(body string, old bool) string {
	keep, drop := regexp.MustCompile(`\[-(?s:(.*?))-\]`), regexp.MustCompile(`\{\+(?s:(.*?))\+\}`)
	if !old {
		keep, drop = drop, keep
	}
	return keep.ReplaceAllString(drop.ReplaceAllString(body, ""), "$1")
}

// TestWordPatchRebuildsBothSides: a word patch whose one hunk covers the
// whole file gives back each side exactly when its marks are resolved.
func TestWordPatchRebuildsBothSides(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	words := []string{"alpha", "beta", "gamma", "delta", "x", "Middle", "hub"}
	gen := func() string {
		var sb strings.Builder
		n := rng.Intn(6)
		for l := 0; l < n; l++ {
			m := 1 + rng.Intn(12)
			for w := 0; w < m; w++ {
				if w > 0 {
					sb.WriteString([]string{" ", " ", "  ", "\t"}[rng.Intn(4)])
				}
				sb.WriteString(words[rng.Intn(len(words))])
			}
			sb.WriteString("\n")
		}
		return sb.String()
	}
	checked := 0
	for i := 0; i < 2000; i++ {
		a, b := gen(), gen()
		p := textPatch("a/f", "b/f", a, b, true)
		if a == b {
			if p != "" {
				t.Fatalf("equal texts gave a patch")
			}
			continue
		}
		// Short texts fit in one hunk that covers the whole file.
		if len(splitLines(a)) > 6 || len(splitLines(b)) > 6 {
			continue
		}
		lines := strings.SplitN(p, "\n", 4)
		if !strings.HasPrefix(lines[2], "@@ ") || strings.Count(p, "\n@@ ") != 1 {
			continue
		}
		body := lines[3]
		// A text that ends without a newline gets one for display, with a
		// note; neither is part of either side.
		body = strings.TrimSuffix(body, "\n\\ No newline at end of file\n")
		if gotA := unmark(body, true); gotA != a {
			t.Fatalf("old side:\na=%q\nb=%q\npatch=%q\ngot %q", a, b, p, gotA)
		}
		if gotB := unmark(body, false); gotB != b {
			t.Fatalf("new side:\na=%q\nb=%q\npatch=%q\ngot %q", a, b, p, gotB)
		}
		checked++
	}
	if checked < 1000 {
		t.Fatalf("only %d of 2000 cases had a single whole-file hunk", checked)
	}
}
