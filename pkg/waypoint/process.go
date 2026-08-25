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

	if err := process.Signal(syscall.SIGTERM); err != nil {
		// If graceful termination fails, try SIGKILL
		if err := process.Signal(syscall.SIGKILL); err != nil {
			return fmt.Errorf("failed to kill process %d: %w", pid, err)
		}
	}

	// Wait for process to terminate (up to 5 seconds).
	for i := 0; i < 50; i++ {
		if !m.processExists(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	// If still running, force kill
	return process.Signal(syscall.SIGKILL)
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

	holders, err := m.findPidHolders(taskIDs)
	if err != nil {
		return fmt.Errorf("failed to scan for processes holding checkpoint task IDs: %w", err)
	}
	for holderPID, held := range holders {
		fmt.Printf("Process %d keeps checkpoint %s reserved and must go before the restore\n",
			holderPID, strings.Join(held, " and "))
		pidsToKill[holderPID] = struct{}{}
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

// parseProcStatIDs pulls the process group id and session id out of the
// contents of /proc/<pid>/stat.
//
// The comm field is parenthesised and may itself contain spaces and
// parentheses, so the fixed-position fields are only meaningful after the
// final ')'. Counting from there: 1=state, 2=ppid, 3=pgrp, 4=session.
func parseProcStatIDs(stat string) (pgrp int, session int, err error) {
	end := strings.LastIndex(stat, ")")
	if end < 0 {
		return 0, 0, fmt.Errorf("malformed stat line: no comm field")
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 4 {
		return 0, 0, fmt.Errorf("malformed stat line: only %d fields after comm", len(fields))
	}
	if pgrp, err = strconv.Atoi(fields[2]); err != nil {
		return 0, 0, fmt.Errorf("unparsable pgrp %q: %w", fields[2], err)
	}
	if session, err = strconv.Atoi(fields[3]); err != nil {
		return 0, 0, fmt.Errorf("unparsable session %q: %w", fields[3], err)
	}
	return pgrp, session, nil
}

// findPidHolders reports live processes that keep one of the checkpoint's task
// IDs allocated *without* owning a task by that ID.
//
// A pid stays reserved in the kernel for as long as anything still references
// it, and a process group id or session id counts as a reference. So a single
// surviving grandchild of a checkpointed shell -- a job that was backgrounded
// and then reparented to init, say -- keeps the shell's pid taken long after
// /proc/<pid> is gone. CRIU restores tasks with clone3(set_tid=...), which
// fails with EEXIST on a reserved pid, so the restore dies with a bare
// "Can't fork for <pid>: File exists" that no /proc lookup can predict.
//
// The returned map is keyed by the holding process, valued by a description of
// what it is holding, so callers can explain themselves before killing it.
func (m *Manager) findPidHolders(taskIDs []int) (map[int][]string, error) {
	wanted := make(map[int]struct{}, len(taskIDs))
	for _, taskID := range taskIDs {
		wanted[taskID] = struct{}{}
	}

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("failed to read /proc: %w", err)
	}

	holders := make(map[int][]string)
	for _, entry := range entries {
		holderPID, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue // not a process directory
		}
		if _, isCheckpointTask := wanted[holderPID]; isCheckpointTask {
			// Owned outright, not merely referenced; findTaskOwnerPID covers it.
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if err != nil {
			continue // exited between ReadDir and here
		}
		pgrp, session, err := parseProcStatIDs(string(data))
		if err != nil {
			continue
		}
		if _, held := wanted[session]; held {
			holders[holderPID] = append(holders[holderPID], fmt.Sprintf("session %d", session))
		}
		if _, held := wanted[pgrp]; held && pgrp != session {
			holders[holderPID] = append(holders[holderPID], fmt.Sprintf("process group %d", pgrp))
		}
	}
	return holders, nil
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

	holders, err := m.findPidHolders(taskIDs)
	if err != nil {
		return nil, err
	}
	for holderPID, held := range holders {
		conflicts = append(conflicts, fmt.Sprintf("%s reserved by live process %d",
			strings.Join(held, " and "), holderPID))
	}

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
	data, err := os.ReadFile(statusPath)
	if err != nil {
		return 0, err
	}

	prefix := field + ":"
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		return strconv.Atoi(value)
	}

	return 0, fmt.Errorf("field %s not found in %s", field, statusPath)
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
