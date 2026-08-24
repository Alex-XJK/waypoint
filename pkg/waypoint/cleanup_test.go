package waypoint

import (
	"strings"
	"testing"
)

func TestSortMountsDeepestFirst(t *testing.T) {
	// The long-sibling case: sorting by path length puts /a/bbbbbbbb ahead of
	// its own children, so the parent is unmounted first and every submount
	// unmount then fails.
	mounts := []string{
		"/s/work",
		"/s/work/proc",
		"/s/work/sys",
		"/s/wo",
		"/s/wo/x/y/z",
		"/s/aaaaaaaaaaaaaaaaaaaa",
	}
	sortMountsDeepestFirst(mounts)

	pos := make(map[string]int, len(mounts))
	for i, m := range mounts {
		pos[m] = i
	}
	for _, child := range mounts {
		for _, parent := range mounts {
			if child == parent || !strings.HasPrefix(child, parent+"/") {
				continue
			}
			if pos[child] > pos[parent] {
				t.Fatalf("submount %s sorted after its parent %s: %v", child, parent, mounts)
			}
		}
	}
}

func TestSortMountsDeepestFirstIsDeterministic(t *testing.T) {
	a := []string{"/s/b", "/s/a", "/s/c/d", "/s/c"}
	b := []string{"/s/c", "/s/c/d", "/s/a", "/s/b"}
	sortMountsDeepestFirst(a)
	sortMountsDeepestFirst(b)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("order depends on input: %v vs %v", a, b)
		}
	}
}
