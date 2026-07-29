package waypoint

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// startShellWithChild launches a shell that backgrounds a long-lived child, the
// shape bash_init creates when it starts its inner bash. Returns the shell PID.
func startShellWithChild(t *testing.T) (*Manager, int) {
	t.Helper()

	cmd := exec.Command("sh", "-c", "sleep 300 & sleep 300")
	// Setsid mirrors bash_init: the child lands in its own session, so killing
	// the parent's process group does not reach it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start test shell: %v", err)
	}

	m := &Manager{}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = m.killProcessTree(pid)
		_ = cmd.Wait()
	})

	// Give the shell a moment to fork its children.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tree, err := collectProcessTree(pid); err == nil && len(tree) >= 3 {
			return m, pid
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("test shell never spawned its children")
	return nil, 0
}

func TestCollectProcessTreeFindsDescendants(t *testing.T) {
	_, pid := startShellWithChild(t)

	tree, err := collectProcessTree(pid)
	if err != nil {
		t.Fatalf("collectProcessTree: %v", err)
	}
	if tree[0] != pid {
		t.Errorf("tree[0] = %d, want root %d", tree[0], pid)
	}
	if len(tree) < 3 {
		t.Errorf("tree = %v, want the shell plus its two children", tree)
	}

	seen := map[int]bool{}
	for _, p := range tree {
		if seen[p] {
			t.Fatalf("duplicate pid %d in tree %v", p, tree)
		}
		seen[p] = true
	}
}

// TestKillProcessLeavesDescendants documents why killProcessTree exists: killing
// only the recorded PID orphans the children, and those keep the overlay busy.
func TestKillProcessLeavesDescendants(t *testing.T) {
	m, pid := startShellWithChild(t)

	tree, err := collectProcessTree(pid)
	if err != nil {
		t.Fatalf("collectProcessTree: %v", err)
	}
	if err := m.killProcess(pid); err != nil {
		t.Fatalf("killProcess: %v", err)
	}

	var survivors []int
	for _, p := range tree[1:] {
		if m.processExists(p) {
			survivors = append(survivors, p)
		}
	}
	if len(survivors) == 0 {
		t.Skip("children did not outlive the parent on this system")
	}
	t.Logf("killProcess(%d) left %v running", pid, survivors)

	for _, p := range survivors {
		_ = m.killProcess(p)
	}
}

func TestKillProcessTreeKillsEverything(t *testing.T) {
	m, pid := startShellWithChild(t)

	tree, err := collectProcessTree(pid)
	if err != nil {
		t.Fatalf("collectProcessTree: %v", err)
	}

	if err := m.killProcessTree(pid); err != nil {
		t.Fatalf("killProcessTree: %v", err)
	}

	for _, p := range tree {
		if !m.processGone(p) {
			t.Errorf("pid %d survived killProcessTree(%d)", p, pid)
		}
	}
}

func TestProcessGoneTreatsZombieAsGone(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Wait() })

	m := &Manager{}
	// The process exits immediately but is never reaped, so it lingers as a
	// zombie: still signalable, but holding nothing.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m.processGone(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("processGone(%d) never became true for an exited process", pid)
}

func TestSelfAncestryProtectsCurrentProcess(t *testing.T) {
	ancestry := selfAncestry()
	if _, ok := ancestry[os.Getpid()]; !ok {
		t.Error("selfAncestry must include the current process")
	}
	if len(ancestry) < 2 {
		t.Errorf("selfAncestry = %v, want the test process plus at least one parent", ancestry)
	}
}

func TestFindProcessesRootedInSkipsSelf(t *testing.T) {
	m := &Manager{}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}

	// The test binary's cwd is inside this tree, yet it must not be reported.
	pids, err := m.findProcessesRootedIn(cwd)
	if err != nil {
		t.Fatalf("findProcessesRootedIn: %v", err)
	}
	for _, pid := range pids {
		if pid == os.Getpid() {
			t.Error("findProcessesRootedIn returned the calling process")
		}
	}

	if pids, err := m.findProcessesRootedIn(""); err != nil || pids != nil {
		t.Errorf("findProcessesRootedIn(\"\") = %v, %v; want nil, nil", pids, err)
	}
}
