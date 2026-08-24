package waypoint

// Session teardown: the graceful, forced, and interactive cleanup flows and
// their supporting machinery — identity-checked kills (pidfd + start time,
// immune to PID reuse), /proc-walk straggler discovery, and
// mountinfo-driven unmounting. No external binaries involved.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Cleanup removes all files and unmounts the overlay for this session.
func (m *Manager) Cleanup() error {
	loadConfig()

	// Cleanup shell related resources if shell enabled
	if m.shellPid != ShellNotEnabled {
		if err := killTree(m.shellPid, m.shellStartTime); err != nil {
			fmt.Printf("Warning: Failed to kill shell process: %v\n", err)
		} else if m.shellSocket != "" {
			// Ignore errors - might already be removed
			os.Remove(m.shellSocket)
		}
	}
	if forks, err := m.ListForks(); err == nil {
		for _, f := range forks {
			if err := m.DestroyFork(f.ID); err != nil {
				fmt.Printf("Warning: Failed to destroy fork %s: %v\n", f.ID, err)
			}
		}
	}

	unmountAll(sessionMounts(m.baseDir))

	if PreserveSessionOnCleanup {
		m.preserveTmpfsImages()
		fmt.Printf("Preserving session directory and session info for %s\n", m.sessionID)
		return nil
	}
	m.removeTmpfsImages()

	if err := os.RemoveAll(m.baseDir); err != nil {
		return fmt.Errorf("failed to remove session directory: %w", err)
	}
	return removeSessionInfo(m.sessionID)
}

// Suspend ends all of a session's live compute while leaving every durable
// artifact on disk: running forks are destroyed (their processes killed and
// their un-snapshotted divergence discarded — checkpoints are the only
// durable state; snapshot first to keep a fork's latest state), stragglers
// and mounts are swept, and pending tmpfs images are flushed to disk so the
// checkpoint DAG survives a reboot. The session stays registered; any
// checkpoint can be forked again later.
func (m *Manager) Suspend() error {
	loadConfig()

	if forks, err := m.ListForks(); err == nil {
		for _, f := range forks {
			switch f.Status {
			case ForkStatusRunning, ForkStatusStarting, ForkStatusSnapshot:
				if err := m.DestroyFork(f.ID); err != nil {
					fmt.Printf("Warning: failed to destroy fork %s: %v\n", f.ID, err)
				}
			}
		}
	}
	m.killStragglers()
	unmountAll(sessionMounts(m.baseDir))

	m.preserveTmpfsImages()

	m.shellPid = ShellNotEnabled
	m.shellStartTime = 0
	m.shellSocket = ""
	return saveSessionInfo(m.sessionID, m)
}

// CleanupForce tears the session down even when processes or mounts are
// still holding it: kill holders, close handles, force-unmount, then remove.
func (m *Manager) CleanupForce() error {
	loadConfig()

	fmt.Printf("Starting forceful cleanup for session %s...\n", m.sessionID)

	fmt.Println("Killing session processes...")
	if forks, err := m.ListForks(); err == nil {
		for _, f := range forks {
			if f.PID > 0 {
				_ = killTree(f.PID, f.StartTime)
			}
		}
	}
	m.killStragglers()

	fmt.Println("Unmounting session mounts...")
	unmountAll(sessionMounts(m.baseDir))

	if PreserveSessionOnCleanup {
		m.preserveTmpfsImages()
		fmt.Printf("Preserving session directory and session info for %s\n", m.sessionID)
		return nil
	}
	m.removeTmpfsImages()

	fmt.Println("Removing session directory...")
	if err := m.removeDirectoryWithRetry(); err != nil {
		// Must error out if we cannot remove the directory, otherwise we might leave a broken session
		return fmt.Errorf("failed to remove session directory after multiple attempts: %w", err)
	}

	fmt.Println("Removing session info...")
	if err := removeSessionInfo(m.sessionID); err != nil {
		fmt.Printf("Warning: Failed to remove session info: %v\n", err)
	}

	return nil
}

// flushAllCheckpointImages synchronously flushes every checkpoint whose
// CRIU images still live on tmpfs to its durable criu.disk dir (a no-op per
// checkpoint that is already on disk).
func (m *Manager) flushAllCheckpointImages() error {
	entries, err := os.ReadDir(filepath.Join(m.baseDir, "checkpoints"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var firstErr error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if validateCheckpointID(e.Name()) != nil {
			continue // not a checkpoint this build could have created
		}
		if err := m.FlushCheckpointImages(e.Name()); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("checkpoint %s: %w", e.Name(), err)
		}
	}
	return firstErr
}

// preserveTmpfsImages makes checkpoint images durable on paths that keep the
// session's disk state (suspend, preserve-mode cleanup): flush everything to
// disk, and only then drop the session's tmpfs dir. If a flush fails, the
// tmpfs copies are left in place — still restorable until reboot — instead
// of being deleted out from under their checkpoints.
func (m *Manager) preserveTmpfsImages() {
	if err := m.flushAllCheckpointImages(); err != nil {
		fmt.Printf("Warning: could not flush all checkpoint images to disk; leaving tmpfs copies in place: %v\n", err)
		return
	}
	m.removeTmpfsImages()
}

// removeTmpfsImages drops this session's tmpfs image dirs (checkpoints not
// yet — or never — flushed to disk). Best-effort: the dir may not exist.
func (m *Manager) removeTmpfsImages() {
	if m.sessionID == "" {
		return
	}
	if err := os.RemoveAll(m.sessionTmpfsDir()); err != nil {
		fmt.Printf("Warning: failed to remove tmpfs images dir: %v\n", err)
	}
}

// CleanupInteractive tries the graceful cleanup, then prints troubleshooting
// hints (holding processes, live mounts) if it fails.
func (m *Manager) CleanupInteractive() error {
	err := m.Cleanup()
	if err == nil {
		return nil
	}

	fmt.Printf("Automatic cleanup failed: %v\n", err)
	fmt.Println("This usually happens when processes are still using files in the session directory.")
	fmt.Println("\nTroubleshooting hints:")

	fmt.Printf("Processes using session directory:\n")
	pids := findSessionProcesses(m.baseDir)
	if len(pids) > 0 {
		for _, pid := range pids {
			fmt.Printf("  %d  %s\n", pid, procCmdline(pid))
		}
	} else {
		fmt.Print("  No processes found")
	}
	fmt.Println()

	fmt.Printf("Active mount points:\n")
	mounts := sessionMounts(m.baseDir)
	if len(mounts) > 0 {
		for _, mount := range mounts {
			fmt.Printf("  %s\n", mount)
		}
	} else {
		fmt.Print("  No mounts found")
	}
	fmt.Println()

	fmt.Println("\nRecommended actions:")
	fmt.Println("1. Close any terminals/editors in the session directory")
	fmt.Println("2. Deactivate Python virtual environments, Docker containers, etc.")
	fmt.Println("3. Stop any processes listed above")
	fmt.Println("4. Unmount any mounts listed above (e.g., using 'sudo umount <mountpoint>')")

	return fmt.Errorf("manual intervention required")
}

// --- process identity + kill ---

// procStartTime returns the process's start time (field 22 of
// /proc/<pid>/stat, jiffies since boot). (pid, starttime) uniquely
// identifies a process incarnation — a recycled PID never repeats the pair —
// so recording it at spawn makes later kills reuse-safe.
func procStartTime(pid int) (uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	// comm (field 2) may contain spaces and parens; the numeric fields
	// resume after the last ')'.
	rest := string(data)
	i := strings.LastIndexByte(rest, ')')
	if i < 0 {
		return 0, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	fields := strings.Fields(rest[i+1:])
	const startTimeIndex = 19 // field 22; fields[0] here is field 3 (state)
	if len(fields) <= startTimeIndex {
		return 0, fmt.Errorf("short /proc/%d/stat", pid)
	}
	return strconv.ParseUint(fields[startTimeIndex], 10, 64)
}

func processExists(pid int) bool {
	return unix.Kill(pid, 0) == nil
}

// killTree SIGKILLs the recorded (pid, startTime) identity and waits for the
// process to disappear. Targets are namespace inits (bash_init or a restored
// tree's root), so their death tears down every descendant. There is no
// SIGTERM grace on purpose: a namespace init with no handlers discards
// SIGTERM, so the old graceful phase only ever added a 5s stall before the
// inevitable SIGKILL. startTime 0 means "no recorded identity" (legacy
// session records): the kill proceeds without reuse protection, as before.
func killTree(pid int, startTime uint64) error {
	if pid <= 0 {
		return nil
	}
	pfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		if err == unix.ESRCH {
			return nil // already gone
		}
		return killTreeByPid(pid, startTime) // kernel without pidfd support
	}
	defer unix.Close(pfd)
	// The pidfd pins this incarnation; if the PID was recycled before the
	// open, the start-time check catches it and nothing gets signaled.
	if startTime != 0 {
		if st, err := procStartTime(pid); err != nil || st != startTime {
			return nil
		}
	}
	if err := unix.PidfdSendSignal(pfd, unix.SIGKILL, nil, 0); err != nil && err != unix.ESRCH {
		return fmt.Errorf("kill pid %d: %w", pid, err)
	}
	return awaitExit(pid)
}

// killTreeByPid is killTree for kernels without pidfd_open.
func killTreeByPid(pid int, startTime uint64) error {
	if startTime != 0 {
		if st, err := procStartTime(pid); err != nil || st != startTime {
			return nil
		}
	}
	if err := unix.Kill(pid, unix.SIGKILL); err != nil {
		if err == unix.ESRCH {
			return nil
		}
		return fmt.Errorf("kill pid %d: %w", pid, err)
	}
	return awaitExit(pid)
}

// awaitExit waits for the PID to vanish entirely: CRIU needs a checkpointed
// task fully reaped before its resources can be reused. SIGKILL death is
// near-immediate and the tree is reparented to init (prompt reaping), so the
// tight poll normally ends within a tick.
func awaitExit(pid int) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processExists(pid) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("process %d still present 5s after SIGKILL", pid)
}

// --- straggler discovery (force cleanup) ---

// findSessionProcesses returns PIDs whose root, cwd, exe, or any open fd
// resolves under baseDir. Replaces `lsof +D` (slow full-tree scan, external
// dependency) and `fuser -k` (which killed anything touching the directory,
// including e.g. a shell merely cd'd into it... after listing it here).
func findSessionProcesses(baseDir string) []int {
	base := filepath.Clean(baseDir)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self || pid == 1 {
			continue
		}
		if procUsesDir(pid, base) {
			pids = append(pids, pid)
		}
	}
	return pids
}

func procUsesDir(pid int, base string) bool {
	proc := "/proc/" + strconv.Itoa(pid)
	for _, link := range []string{"root", "cwd", "exe"} {
		if pathUnder(readLinkTarget(proc+"/"+link), base) {
			return true
		}
	}
	fds, err := os.ReadDir(proc + "/fd")
	if err != nil {
		return false
	}
	for _, fd := range fds {
		if pathUnder(readLinkTarget(proc+"/fd/"+fd.Name()), base) {
			return true
		}
	}
	return false
}

func readLinkTarget(path string) string {
	target, err := os.Readlink(path)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(target, " (deleted)")
}

func pathUnder(path, base string) bool {
	return path != "" && (path == base || strings.HasPrefix(path, base+"/"))
}

// procCmdline returns a process's argv as one line (best-effort, for
// reporting).
func procCmdline(pid int) string {
	data, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	return strings.TrimSpace(strings.ReplaceAll(string(data), "\x00", " "))
}

// killStragglers force-kills whatever session processes remain. The match is
// re-checked after the pidfd pins the incarnation, so a PID recycled between
// discovery and kill is left alone.
func (m *Manager) killStragglers() {
	base := filepath.Clean(m.baseDir)
	for _, pid := range findSessionProcesses(base) {
		pfd, err := unix.PidfdOpen(pid, 0)
		if err != nil {
			if err != unix.ESRCH && procUsesDir(pid, base) {
				_ = unix.Kill(pid, unix.SIGKILL) // kernel without pidfds
			}
			continue
		}
		if procUsesDir(pid, base) {
			fmt.Printf("Killing leftover process %d (%s)\n", pid, procCmdline(pid))
			_ = unix.PidfdSendSignal(pfd, unix.SIGKILL, nil, 0)
		}
		unix.Close(pfd)
	}
}

// --- unmount machinery ---

// sessionMounts lists mount points at or under baseDir, deepest first,
// parsed from /proc/self/mountinfo. (findmnt -M matched only the exact
// mountpoint, missing submounts, and cost a subprocess.)
func sessionMounts(baseDir string) []string {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	base := filepath.Clean(baseDir)
	var mounts []string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if mp := unescapeMountInfoPath(fields[4]); pathUnder(mp, base) {
			mounts = append(mounts, mp)
		}
	}
	sortMountsDeepestFirst(mounts)
	return mounts
}

// sortMountsDeepestFirst orders mount points so no parent is unmounted before
// its submounts. Depth is the number of separators, not the path length:
// siblings like /a/bbbbbbbb and /a/b/c sort the wrong way round by length.
// Equal depths fall back to reverse lexicographic order so the sweep is
// deterministic.
func sortMountsDeepestFirst(mounts []string) {
	sort.Slice(mounts, func(i, j int) bool {
		di, dj := strings.Count(mounts[i], "/"), strings.Count(mounts[j], "/")
		if di != dj {
			return di > dj
		}
		return mounts[i] > mounts[j]
	})
}

// unmountAll unmounts the given mount points in order, escalating EBUSY to a
// lazy detach. Only paths mountinfo reported as mounts are attempted, so
// "not mounted" noise cannot occur.
func unmountAll(mounts []string) {
	for _, mp := range mounts {
		err := unix.Unmount(mp, 0)
		if err == nil || err == unix.ENOENT || err == unix.EINVAL {
			continue
		}
		if err := unix.Unmount(mp, unix.MNT_DETACH); err != nil && err != unix.ENOENT && err != unix.EINVAL {
			fmt.Printf("Warning: failed to unmount %s: %v\n", mp, err)
		}
	}
}

// removeDirectoryWithRetry attempts to remove the base directory with exponential backoff
func (m *Manager) removeDirectoryWithRetry() error {
	maxAttempts := 5
	delay := 500 * time.Millisecond

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err := os.RemoveAll(m.baseDir)
		if err == nil {
			return nil
		}
		if attempt == maxAttempts {
			return fmt.Errorf("final attempt failed: %w", err)
		}

		fmt.Printf("Attempt %d failed (%v), retrying in %v...\n", attempt, err, delay)
		time.Sleep(delay)
		delay *= 2
	}
	return nil
}
