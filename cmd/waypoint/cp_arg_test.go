package main

import "testing"

func TestSplitForkArg(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		wantFork   string
		wantPath   string
		wantIsFork bool
	}{
		{"plain fork ref", "fork-abc:/work/f.txt", "fork-abc", "/work/f.txt", true},
		{"main ref", "main:/work", "main", "/work", true},
		{"absolute host path", "/tmp/x/y.txt", "", "/tmp/x/y.txt", false},
		{"host path with colon in a dir component", "/a/b:c/y.txt", "", "/a/b:c/y.txt", false},
		{"host path with colon in basename", "/tmp/we:ird.txt", "", "/tmp/we:ird.txt", false},
		{"relative host path no colon", "rel/path.txt", "", "rel/path.txt", false},
		{"leading colon is not a fork", ":/work", "", ":/work", false},
		{"fork id with a colon-less relative-looking path", "main:work/f", "main", "work/f", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fork, path, isFork := splitForkArg(tc.in)
			if fork != tc.wantFork || path != tc.wantPath || isFork != tc.wantIsFork {
				t.Fatalf("splitForkArg(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.in, fork, path, isFork, tc.wantFork, tc.wantPath, tc.wantIsFork)
			}
		})
	}
}
