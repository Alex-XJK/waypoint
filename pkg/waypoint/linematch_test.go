package waypoint

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
)

// lcsLength is the reference: the textbook O(NM) dynamic program.
func lcsLength(a, b []string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			switch {
			case a[i-1] == b[j-1]:
				cur[j] = prev[j-1] + 1
			case prev[j] >= cur[j-1]:
				cur[j] = prev[j]
			default:
				cur[j] = cur[j-1]
			}
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// checkMatch verifies that match pairs equal lines in increasing order and returns their count.
func checkMatch(t *testing.T, before, after []string, match []int) int {
	t.Helper()
	if len(match) != len(after) {
		t.Fatalf("match has %d entries for %d lines", len(match), len(after))
	}
	kept, last := 0, -1
	for j, i := range match {
		if i < 0 {
			continue
		}
		if i <= last || i >= len(before) {
			t.Fatalf("match %v is not increasing within before (line %d -> %d)", match, j, i)
		}
		if before[i] != after[j] {
			t.Fatalf("line %d of after (%q) matched to line %d of before (%q)", j, after[j], i, before[i])
		}
		last = i
		kept++
	}
	return kept
}

func randomLines(r *rand.Rand, n, alphabet int) []string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("l%d", r.Intn(alphabet))
	}
	return lines
}

func TestMatchLines_MinimalOnRandomInputs(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for iter := 0; iter < 3000; iter++ {
		before := randomLines(r, r.Intn(40), 1+r.Intn(6))
		after := randomLines(r, r.Intn(40), 1+r.Intn(6))
		match, exact := matchLines(before, after)
		if !exact {
			t.Fatalf("small input reported approximate")
		}
		if got, want := checkMatch(t, before, after, match), lcsLength(before, after); got != want {
			t.Fatalf("kept %d lines, longest common subsequence has %d\nbefore %v\nafter  %v\nmatch  %v",
				got, want, before, after, match)
		}
	}
}

func TestMatchLines_EditsOfOneFile(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	for iter := 0; iter < 300; iter++ {
		before := randomLines(r, 200+r.Intn(200), 1000)
		after := append([]string(nil), before...)
		for e := r.Intn(80); e > 0; e-- { // scattered inserts and deletes
			p := r.Intn(len(after) + 1)
			if r.Intn(2) == 0 && p < len(after) {
				after = append(after[:p], after[p+1:]...)
			} else {
				after = append(after[:p], append([]string{fmt.Sprintf("new%d", r.Int())}, after[p:]...)...)
			}
		}
		match, exact := matchLines(before, after)
		if !exact {
			t.Fatalf("reported approximate")
		}
		if got, want := checkMatch(t, before, after, match), lcsLength(before, after); got != want {
			t.Fatalf("kept %d lines, want %d", got, want)
		}
	}
}

func TestMatchLines_Cases(t *testing.T) {
	for _, tc := range []struct {
		before, after []string
		want          []int
	}{
		{nil, nil, []int{}},
		{nil, []string{"a"}, []int{-1}},
		{[]string{"a"}, nil, []int{}},
		{[]string{"a", "b", "c"}, []string{"a", "b", "c"}, []int{0, 1, 2}},
		{[]string{"a", "b", "c"}, []string{"a", "x", "c"}, []int{0, -1, 2}},
		{[]string{"a", "b"}, []string{"x", "y"}, []int{-1, -1}},
		{[]string{"a", "b", "c"}, []string{"c", "a"}, []int{-1, 0}},
	} {
		got, exact := matchLines(tc.before, tc.after)
		if !exact || !reflect.DeepEqual(got, tc.want) {
			// a different longest common subsequence is acceptable; check length
			if len(tc.want) != len(got) || checkMatch(t, tc.before, tc.after, got) != lcsLength(tc.before, tc.after) {
				t.Errorf("matchLines(%v, %v) = %v, want %v", tc.before, tc.after, got, tc.want)
			}
		}
	}
}

func TestMatchLines_BudgetFallsBackToBoundedDiff(t *testing.T) {
	saved := lineMatchBudget
	lineMatchBudget = 10
	defer func() { lineMatchBudget = saved }()

	r := rand.New(rand.NewSource(3))
	before := randomLines(r, 300, 5)
	after := randomLines(r, 300, 5)
	match, exact := matchLines(before, after)
	if exact {
		t.Fatalf("expected the budget to run out")
	}
	checkMatch(t, before, after, match)
}

func TestSplitLines(t *testing.T) {
	for in, want := range map[string][]string{
		"":       nil,
		"a":      {"a"},
		"a\n":    {"a\n"},
		"a\nb":   {"a\n", "b"},
		"a\n\n":  {"a\n", "\n"},
		"\n":     {"\n"},
		"a\r\nb": {"a\r\n", "b"},
	} {
		if got := splitLines(in); !reflect.DeepEqual(got, want) {
			t.Errorf("splitLines(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMatchLines_PrefersCodeOverBlankLines(t *testing.T) {
	before := []string{"def attack():\n", "\n", "\n"}
	after := []string{"\n", "\n", "def attack():\n"}
	// A plain longest common subsequence keeps the two blank lines; blame should keep the code.
	match, exact := matchLines(before, after)
	if want := []int{-1, -1, 0}; !exact || !reflect.DeepEqual(match, want) {
		t.Errorf("matchLines = %v, want %v", match, want)
	}
}

func TestMatchLines_NonBlankLinesMaximalWithBlanks(t *testing.T) {
	r := rand.New(rand.NewSource(4))
	nonBlank := func(lines []string) []string {
		var out []string
		for _, l := range lines {
			if l != "" && l != "  " {
				out = append(out, l)
			}
		}
		return out
	}
	for iter := 0; iter < 3000; iter++ {
		gen := func() []string {
			lines := randomLines(r, r.Intn(40), 1+r.Intn(5))
			for i := range lines {
				switch r.Intn(4) {
				case 0:
					lines[i] = ""
				case 1:
					lines[i] = "  "
				}
			}
			return lines
		}
		before, after := gen(), gen()
		match, exact := matchLines(before, after)
		if !exact {
			t.Fatal("small input reported approximate")
		}
		checkMatch(t, before, after, match)
		keptNonBlank := 0
		for j, i := range match {
			if i >= 0 && after[j] != "" && after[j] != "  " {
				keptNonBlank++
			}
		}
		if want := lcsLength(nonBlank(before), nonBlank(after)); keptNonBlank != want {
			t.Fatalf("kept %d non-blank lines, want %d\nbefore %q\nafter  %q\nmatch  %v", keptNonBlank, want, before, after, match)
		}
	}
}
