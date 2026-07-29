package waypoint

// Process management utilities

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func (m *Manager) processExists(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	// Send signal 0 to check if process exists
	err = process.Signal(syscall.Signal(0))
	return err == nil
}

func (m *Manager) killProcess(pid int) error {
	// Mimic my "__kill_original_process"'s soft and hard kill behavior
	if !m.processExists(pid) {
		// Process does not exist, probably already terminated
		return nil
	}

	// Retrieve the process
	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("failed to retrieve process %d: %w", pid, err)
	}

	// Ask politely first. SIGTERM practically never fails against a live
	// process, so escalation has to be driven by the process still being alive
	// after the grace period rather than by this call returning an error.
	if err := process.Signal(syscall.SIGTERM); err != nil {
		if err := process.Signal(syscall.SIGKILL); err != nil {
			return fmt.Errorf("failed to kill process %d: %w", pid, err)
		}
	}

	// Wait for process to terminate. CRIU restore needs the checkpointed task
	// IDs to disappear before it can reuse them.
	if m.waitForExit(pid, processExitTimeout) {
		return nil
	}

	// Still alive after the grace period: escalate, then wait again. Returning
	// as soon as SIGKILL is delivered would let callers unmount while the
	// process still holds the overlay as its root or cwd.
	if err := process.Signal(syscall.SIGKILL); err != nil {
		if m.processGone(pid) {
			return nil
		}
		return fmt.Errorf("failed to force kill process %d: %w", pid, err)
	}
	if !m.waitForExit(pid, processExitTimeout) {
		return fmt.Errorf("process %d is still alive after SIGKILL", pid)
	}

	return nil
}

// waitForExit polls until pid is gone or timeout elapses, reporting whether it
// actually went away.
func (m *Manager) waitForExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if m.processGone(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(processPollInterval)
	}
}

// processGone reports whether pid has released the resources teardown cares
// about. A zombie counts as gone: it holds no mounts or file descriptors and
// only leaves the process table once its parent reaps it, which never happens
// for the orphaned children cleanup kills. Callers that need the PID number
// itself to be free (CRIU restore) must check /proc separately.
func (m *Manager) processGone(pid int) bool {
	if !m.processExists(pid) {
		return true
	}
	state, err := readProcStatusField(filepath.Join("/proc", strconv.Itoa(pid), "status"), "State")
	if err != nil {
		// Disappeared between the two checks, or unreadable; assume still there.
		return os.IsNotExist(err)
	}
	return strings.HasPrefix(state, "Z")
}

// killProcessTree terminates pid and everything currently descending from it.
// bash_init starts its inner bash with Setsid, so that child forms its own
// session and survives a plain kill of the parent, staying alive with the
// overlay as its root and keeping the mount busy forever.
func (m *Manager) killProcessTree(pid int) error {
	if pid <= 0 {
		return nil
	}

	pids, err := collectProcessTree(pid)
	if err != nil {
		// Fall back to killing just the root process.
		return m.killProcess(pid)
	}

	// Deepest first, so a parent cannot spawn a replacement mid-teardown.
	var failures []string
	for i := len(pids) - 1; i >= 0; i-- {
		if err := m.killProcess(pids[i]); err != nil {
			failures = append(failures, fmt.Sprintf("%d (%v)", pids[i], err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("failed to kill process tree of %d: %s", pid, strings.Join(failures, ", "))
	}

	return nil
}

// collectProcessTree returns root followed by every process descending from it,
// breadth-first. The scan has to happen before anything is killed: once a parent
// dies its children are reparented to init and the relationship is lost.
func collectProcessTree(root int) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("failed to scan /proc: %w", err)
	}

	children := make(map[int][]int)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		ppid, err := readProcStatusInt(filepath.Join("/proc", entry.Name(), "status"), "PPid")
		if err != nil {
			continue
		}
		children[ppid] = append(children[ppid], pid)
	}

	ordered := []int{root}
	seen := map[int]struct{}{root: {}}
	for i := 0; i < len(ordered); i++ {
		for _, child := range children[ordered[i]] {
			if _, dup := seen[child]; dup {
				continue
			}
			seen[child] = struct{}{}
			ordered = append(ordered, child)
		}
	}

	return ordered, nil
}

// selfAncestry returns the current process and every process it descends from.
func selfAncestry() map[int]struct{} {
	ancestry := make(map[int]struct{})
	for pid := os.Getpid(); pid > 1; {
		if _, dup := ancestry[pid]; dup {
			break // Defensive: a cycle should be impossible.
		}
		ancestry[pid] = struct{}{}
		ppid, err := readProcStatusInt(filepath.Join("/proc", strconv.Itoa(pid), "status"), "PPid")
		if err != nil {
			break
		}
		pid = ppid
	}
	return ancestry
}

// findProcessesRootedIn returns PIDs whose root, cwd, or executable lives under
// dir. Those are exactly the processes that keep an overlay mount busy. Unlike
// `lsof +D` this reads four symlinks per process instead of walking the session
// tree, which matters because a built rootfs puts a full distro under baseDir.
func (m *Manager) findProcessesRootedIn(dir string) ([]int, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("failed to scan /proc: %w", err)
	}

	// Never kill ourselves or anything we descend from: a user running cleanup
	// from a directory inside the session would otherwise lose their own shell.
	protected := selfAncestry()

	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if _, skip := protected[pid]; skip {
			continue
		}
		for _, link := range []string{"root", "cwd", "exe"} {
			target, err := os.Readlink(filepath.Join("/proc", entry.Name(), link))
			if err != nil {
				continue
			}
			if target == dir || strings.HasPrefix(target, dir+string(os.PathSeparator)) {
				pids = append(pids, pid)
				break
			}
		}
	}

	sort.Ints(pids)
	return pids, nil
}

// prepareCheckpointRestore clears any live tasks that would block CRIU from
// restoring the checkpointed task IDs back into the host PID namespace.
func (m *Manager) prepareCheckpointRestore(rootPID int, criuPath string) error {
	taskIDs, err := m.readCheckpointTaskIDs(criuPath)
	if err != nil {
		if rootPID <= 0 {
			return fmt.Errorf("failed to parse checkpoint task IDs: %w", err)
		}
		if errKill := m.killProcess(rootPID); errKill != nil {
			return fmt.Errorf("failed to parse checkpoint task IDs (%w), and fallback kill of process %d also failed: %w", err, rootPID, errKill)
		}
		return nil
	}

	pidsToKill := make(map[int]struct{})
	for _, taskID := range taskIDs {
		ownerPID, err := m.findTaskOwnerPID(taskID)
		if err != nil {
			return fmt.Errorf("failed to resolve owner of checkpoint task %d: %w", taskID, err)
		}
		if ownerPID > 0 {
			pidsToKill[ownerPID] = struct{}{}
			// fmt.Printf("DEBUG >> Checking kill: [%d] belongs to [%d]\n", taskID, ownerPID)
		}
	}

	killList := make([]int, 0, len(pidsToKill))
	for pid := range pidsToKill {
		killList = append(killList, pid)
	}
	sort.Ints(killList)
	for _, pid := range killList {
		if err := m.killProcess(pid); err != nil {
			return fmt.Errorf("failed to kill blocking process %d: %w", pid, err)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		conflicts, err := m.findConflictingCheckpointTasks(taskIDs)
		if err != nil {
			return fmt.Errorf("failed to verify checkpoint task IDs: %w", err)
		}
		if len(conflicts) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("checkpoint task IDs still exist after cleanup: %s", strings.Join(conflicts, ", "))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (m *Manager) readCheckpointTaskIDs(criuPath string) ([]int, error) {
	type pstreeEntry struct {
		PID     int   `json:"pid"`
		Threads []int `json:"threads"`
	}
	type pstreeImage struct {
		Entries []pstreeEntry `json:"entries"`
	}

	pstreePath := filepath.Join(criuPath, "pstree.img")
	cmd := exec.Command("crit", "show", pstreePath)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("crit show %s failed: %w", pstreePath, err)
	}

	var image pstreeImage
	if err := json.Unmarshal(output, &image); err != nil {
		return nil, fmt.Errorf("failed to decode pstree image %s: %w", pstreePath, err)
	}

	taskIDSet := make(map[int]struct{})
	for _, entry := range image.Entries {
		if entry.PID > 0 {
			taskIDSet[entry.PID] = struct{}{}
		}
		for _, tid := range entry.Threads {
			if tid > 0 {
				taskIDSet[tid] = struct{}{}
			}
		}
	}
	if len(taskIDSet) == 0 {
		return nil, fmt.Errorf("no task IDs found in %s", pstreePath)
	}

	taskIDs := make([]int, 0, len(taskIDSet))
	for taskID := range taskIDSet {
		taskIDs = append(taskIDs, taskID)
	}
	sort.Ints(taskIDs)
	return taskIDs, nil
}

func (m *Manager) findTaskOwnerPID(taskID int) (int, error) {
	statusPath := filepath.Join("/proc", strconv.Itoa(taskID), "status")
	tgid, err := readProcStatusInt(statusPath, "Tgid")
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return tgid, nil
}

func (m *Manager) findConflictingCheckpointTasks(taskIDs []int) ([]string, error) {
	var conflicts []string
	for _, taskID := range taskIDs {
		ownerPID, err := m.findTaskOwnerPID(taskID)
		if err != nil {
			return nil, err
		}
		if ownerPID == 0 {
			continue
		}
		if ownerPID == taskID {
			conflicts = append(conflicts, strconv.Itoa(taskID))
			continue
		}
		conflicts = append(conflicts, fmt.Sprintf("%d(owner %d)", taskID, ownerPID))
	}
	return conflicts, nil
}

func readProcStatusInt(statusPath, field string) (int, error) {
	value, err := readProcStatusField(statusPath, field)
	if err != nil {
		// Returned unwrapped so callers can still use os.IsNotExist.
		return 0, err
	}
	return strconv.Atoi(value)
}

func readProcStatusField(statusPath, field string) (string, error) {
	data, err := os.ReadFile(statusPath)
	if err != nil {
		return "", err
	}

	prefix := field + ":"
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		return strings.TrimSpace(strings.TrimPrefix(line, prefix)), nil
	}

	return "", fmt.Errorf("field %s not found in %s", field, statusPath)
}

// killProcessesUsingDirectory kills processes that have files open in our directory
func (m *Manager) killProcessesUsingDirectory() error {
	pids, err := m.findProcessesUsingDirectory()
	if err != nil {
		return err
	}

	if len(pids) == 0 {
		return nil
	}

	fmt.Printf("Found %d processes using directory, attempting to terminate...\n", len(pids))

	// Concurrently attempt to kill all processes
	totalProcCount := len(pids)
	errProcCount := 0
	errorChan := make(chan error, totalProcCount)
	for _, pid := range pids {
		go func(pid int) {
			if err := m.killProcess(pid); err != nil {
				errorChan <- fmt.Errorf("failed to kill process %d: %w", pid, err)
			} else {
				fmt.Printf("Successfully killed process %d\n", pid)
				errorChan <- nil
			}
		}(pid)
	}

	// Wait for all kill attempts to finish
	for i := 0; i < totalProcCount; i++ {
		if err := <-errorChan; err != nil {
			errProcCount++
		}
	}

	if errProcCount > 0 {
		return fmt.Errorf("failed to kill %d out of %d processes using directory", errProcCount, totalProcCount)
	}

	return nil
}

// findProcessesUsingDirectory uses lsof to find processes with open files in directory
func (m *Manager) findProcessesUsingDirectory() ([]int, error) {
	// Use lsof to find processes with open files in our directory
	cmd := exec.Command("lsof", "+D", m.baseDir)
	output, err := cmd.Output()
	if err != nil {
		// lsof returns a non-zero exit code if no files are found, which is not an error
		return []int{}, nil
	}

	var pids []int
	lines := strings.Split(string(output), "\n")

	// Skip header line, parse PIDs from lsof output
	for _, line := range lines[1:] {
		if line == "" {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) >= 2 {
			if pid, err := strconv.Atoi(fields[1]); err == nil {
				// Avoid duplicates
				found := false
				for _, existingPid := range pids {
					if existingPid == pid {
						found = true
						break
					}
				}
				if !found {
					pids = append(pids, pid)
				}
			}
		}
	}

	return pids, nil
}

// closeFileHandles attempts to close file handles using fuser
func (m *Manager) closeFileHandles() error {
	cmd := exec.Command("fuser", "-k", m.baseDir)
	cmd.Run()

	return nil
}
