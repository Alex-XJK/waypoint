package waypoint

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

func TestInspectForksValidatesRuntimeIdentity(t *testing.T) {
	m := newManager(t.TempDir())
	m.sessionID = "session"

	pid := os.Getpid()
	startTime, err := procStartTime(pid)
	if err != nil {
		t.Fatalf("procStartTime(%d): %v", pid, err)
	}

	forks := []*Fork{
		{
			ID:        "failed",
			RootDir:   m.forkDir("failed"),
			PID:       pid,
			StartTime: startTime,
			Status:    ForkStatusFailed,
		},
		{
			ID:               "live",
			BaseCheckpointID: "A",
			RootDir:          m.forkDir("live"),
			PID:              pid,
			StartTime:        startTime,
			Status:           ForkStatusRunning,
		},
		{
			ID:        "stale",
			RootDir:   m.forkDir("stale"),
			PID:       pid,
			StartTime: startTime + 1,
			Status:    ForkStatusRunning,
		},
		{
			ID:      "unverified",
			RootDir: m.forkDir("unverified"),
			PID:     pid,
			Status:  ForkStatusRunning,
		},
	}
	for _, f := range forks {
		if err := m.saveFork(f); err != nil {
			t.Fatalf("saveFork(%q): %v", f.ID, err)
		}
	}

	got, err := m.InspectForks()
	if err != nil {
		t.Fatalf("InspectForks(): %v", err)
	}
	if len(got) != len(forks) {
		t.Fatalf("len(InspectForks()) = %d, want %d", len(got), len(forks))
	}

	wantOrder := []string{"failed", "live", "stale", "unverified"}
	for i, wantID := range wantOrder {
		if got[i].ID != wantID {
			t.Fatalf("InspectForks()[%d].ID = %q, want %q", i, got[i].ID, wantID)
		}
		if !got[i].Volatile {
			t.Errorf("InspectForks()[%d].Volatile = false, want true", i)
		}
	}

	if got[0].Available || got[0].HostRoot != "" || got[0].UnavailableReason != "fork is not running" {
		t.Errorf("failed fork runtime = %+v, want unavailable non-running fork", got[0])
	}
	wantRoot := fmt.Sprintf("/proc/%d/root", pid)
	if !got[1].Available || got[1].HostRoot != wantRoot || got[1].UnavailableReason != "" {
		t.Errorf("live fork runtime = %+v, want available root %q", got[1], wantRoot)
	}
	if got[1].BaseCheckpointID != "A" {
		t.Errorf("live fork BaseCheckpointID = %q, want A", got[1].BaseCheckpointID)
	}
	if got[2].Available || got[2].HostRoot != "" || got[2].UnavailableReason != "host PID start time does not match" {
		t.Errorf("stale fork runtime = %+v, want unavailable identity mismatch", got[2])
	}
	if got[3].Available || got[3].HostRoot != "" || got[3].UnavailableReason != "fork has no recorded PID start time" {
		t.Errorf("unverified fork runtime = %+v, want unavailable unverified identity", got[3])
	}
}

func TestInspectForksReturnsEmptySliceWithoutForks(t *testing.T) {
	m := newManager(t.TempDir())
	got, err := m.InspectForks()
	if err != nil {
		t.Fatalf("InspectForks(): %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("InspectForks() = %#v, want non-nil empty slice", got)
	}
}

func TestForkRuntimeInfoJSONDoesNotExposeUnavailableHostRoot(t *testing.T) {
	data, err := json.Marshal(ForkRuntimeInfo{
		ID:                "stale",
		Status:            ForkStatusRunning,
		Volatile:          true,
		UnavailableReason: "host process is unavailable",
	})
	if err != nil {
		t.Fatalf("Marshal(ForkRuntimeInfo): %v", err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("Unmarshal(ForkRuntimeInfo): %v", err)
	}
	if got := string(fields["available"]); got != "false" {
		t.Errorf("available JSON = %s, want explicit false", got)
	}
	if _, ok := fields["host_root"]; ok {
		t.Errorf("unavailable ForkRuntimeInfo JSON contains host_root: %s", data)
	}
}
