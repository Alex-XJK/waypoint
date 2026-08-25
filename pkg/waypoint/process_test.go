package waypoint

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestParseProcStatIDs(t *testing.T) {
	cases := []struct {
		name        string
		stat        string
		wantPgrp    int
		wantSession int
	}{
		{
			name:        "ordinary comm",
			stat:        "1234 (bash) S 1 1234 1200 34816 1234 4194304 500 0 0 0",
			wantPgrp:    1234,
			wantSession: 1200,
		},
		{
			// A comm can contain spaces and parentheses; only the final ')'
			// marks where the fixed-position fields begin.
			name:        "comm with spaces and parens",
			stat:        "4321 (weird ) name (x)) R 1 99 77 0 -1 4194560 12 0 0 0",
			wantPgrp:    99,
			wantSession: 77,
		},
		{
			name:        "orphan holding a dead leader's session",
			stat:        "268937 (python3) S 1 268931 109188 0 -1 4194304 9000 0 0 0",
			wantPgrp:    268931,
			wantSession: 109188,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pgrp, session, err := parseProcStatIDs(tc.stat)
			if err != nil {
				t.Fatalf("parseProcStatIDs(%q) returned error: %v", tc.stat, err)
			}
			if pgrp != tc.wantPgrp || session != tc.wantSession {
				t.Fatalf("parseProcStatIDs(%q) = (pgrp %d, session %d), want (%d, %d)",
					tc.stat, pgrp, session, tc.wantPgrp, tc.wantSession)
			}
		})
	}
}

func TestParseProcStatIDsRejectsGarbage(t *testing.T) {
	for _, stat := range []string{"", "no comm field here", "1234 (bash) S 1"} {
		if _, _, err := parseProcStatIDs(stat); err == nil {
			t.Fatalf("parseProcStatIDs(%q) accepted a malformed stat line", stat)
		}
	}
}

// TestFindPidHoldersSeesSessionReference is the regression this fix is about:
// a pid whose /proc entry does not exist can still be reserved by a live
// process that has it as a session id, and CRIU cannot restore onto it.
func TestFindPidHoldersSeesSessionReference(t *testing.T) {
	self := os.Getpid()
	sid, err := unix.Getsid(0)
	if err != nil {
		t.Skipf("cannot read own session id: %v", err)
	}
	if sid == self {
		t.Skip("test process is its own session leader; nothing references a foreign pid")
	}

	m := &Manager{}
	holders, err := m.findPidHolders([]int{sid})
	if err != nil {
		t.Fatalf("findPidHolders returned error: %v", err)
	}
	if _, found := holders[self]; !found {
		t.Fatalf("findPidHolders(%d) missed this process (%d), which holds that session id; got %v",
			sid, self, holders)
	}

	// A task ID that nothing references must come back clean, otherwise the
	// check would kill innocent processes on every checkpoint.
	if holders, err := m.findPidHolders([]int{}); err != nil || len(holders) != 0 {
		t.Fatalf("findPidHolders(nothing) = (%v, %v), want empty", holders, err)
	}
}
