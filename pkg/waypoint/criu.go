package waypoint

// All criu(8) interactions: memory dumps and the restore helper child that
// re-creates a fork's process tree inside fresh namespaces.
//
// Host compatibility (criu present, recent enough, kernel features, and the
// arm64 PAC/CRIU-4.0 requirement) is validated out of band by `./setup check`,
// not at runtime.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// --- dump ---

// createMemoryCheckpoint dumps a process tree with criu. The work overlay is
// declared as an external mount so CRIU treats it as managed by us.
// extraArgs extend the dump command (e.g. --leave-running, --action-script).
// Notice: cannot use '--shell-job' because of the PTY issue during restore.
func (m *Manager) createMemoryCheckpoint(pid int, criuPath string, extraArgs ...string) error {
	args := []string{"dump",
		"-t", strconv.Itoa(pid),
		"-D", criuPath,
		"--tcp-established",
		"--ghost-limit", "8388608",
		"-vv", "-o", "dump.log",
	}
	args = append(args, extraArgs...)
	if _, err := findMountID(pid, m.workOverlay); err == nil {
		args = append(args, "--external", fmt.Sprintf("mnt[%s]:waypoint-work", m.workOverlay))
	} else if _, err := findMountID(pid, "/"); err == nil {
		args = append(args, "--external", "mnt[/]:waypoint-work")
	}

	cmd := exec.Command("criu", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: 0, Gid: 0},
	}

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create memory checkpoint: %w\ncriu stderr: %s", err, stderr.String())
	}
	return nil
}

func findMountID(pid int, mountPoint string) (string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "mountinfo"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if unescapeMountInfoPath(fields[4]) == mountPoint {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("mount %s not found in pid %d mountinfo", mountPoint, pid)
}

func unescapeMountInfoPath(path string) string {
	replacer := strings.NewReplacer(
		`\\`, `\`,
		`\040`, " ",
		`\011`, "\t",
		`\012`, "\n",
	)
	return replacer.Replace(path)
}

// --- restore ---

// runRestoreHelper re-executes this binary as a helper child inside fresh
// mount/net/IPC namespaces; the child mounts the fork's overlay and runs
// `criu restore` there (see RunForkRestoreChildFromArgs).
func runRestoreHelper(f *Fork) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	statePath := filepath.Join(f.RootDir, ForkStateFile)
	cmd := exec.Command(exe, "__waypoint_restore_fork_child", statePath)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: uintptr(unix.CLONE_NEWNS | unix.CLONE_NEWNET | unix.CLONE_NEWIPC),
		Setsid:     true,
	}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("fork restore helper failed: %w\n%s", err, output.String())
	}
	return nil
}

// RunForkRestoreChildFromArgs is the entry point for the hidden
// __waypoint_restore_fork_child CLI subcommand.
func RunForkRestoreChildFromArgs(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: __waypoint_restore_fork_child <fork-state-json>")
	}
	f, err := loadForkFile(args[0])
	if err != nil {
		return err
	}
	return restoreForkChild(f)
}

// restoreTimingFile is where the restore child leaves its phase timings for
// the parent (the helper's stdout is reserved for error reporting).
const restoreTimingFile = "restore-timing.json"

// childRestoreTiming is the helper child's share of the restore breakdown.
type childRestoreTiming struct {
	MountMs    float64 `json:"mount_ms"`
	CriuWallMs float64 `json:"criu_wall_ms"`
}

func restoreForkChild(f *Fork) error {
	mountStart := time.Now()
	if err := prepareForkMountNamespace(f); err != nil {
		return err
	}
	if err := bringLoopbackUp(); err != nil {
		return err
	}
	timing := childRestoreTiming{MountMs: durMs(time.Since(mountStart))}

	args := []string{
		"restore",
		"--images-dir", f.CriuPath,
		"--tcp-established",
		"--restore-detached",
		"--pidfile", f.PidFile,
		// The images dir is shared by all forks of a checkpoint, so anything
		// criu writes by default must be redirected per fork: logs via an
		// absolute -o path, stats-restore via --work-dir.
		"--work-dir", f.RootDir,
		"-vv", "-o", f.LogPath,
	}
	if f.LazyPages {
		args = append(args, "--lazy-pages")
	}
	if f.MountPoint != "" {
		args = append(args, "-r", f.MountPoint)
		args = append(args, "--external", fmt.Sprintf("mnt[waypoint-work]:%s", f.MountPoint))
	}
	cmd := exec.Command("criu", args...)
	cmd.Dir = f.RootDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	// Hold the images lock shared for the whole restore so the tmpfs->disk
	// flusher (exclusive) cannot repoint and delete images mid-read.
	if unlock, err := lockImages(filepath.Dir(f.CriuPath), false); err == nil {
		defer unlock()
	}

	criuStart := time.Now()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("criu restore failed: %w\n%s", err, stderr.String())
	}
	timing.CriuWallMs = durMs(time.Since(criuStart))

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pid, err := readPIDFile(f.PidFile); err == nil && pid > 0 {
			if data, err := json.Marshal(timing); err == nil {
				_ = os.WriteFile(filepath.Join(f.RootDir, restoreTimingFile), data, 0o644)
			}
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("criu restore did not write pidfile %s", f.PidFile)
}

// readChildRestoreTiming loads (and consumes) the timing file the restore
// child left behind; stale files from a previous restore of the same fork
// are prevented by the removal here.
func readChildRestoreTiming(f *Fork) (childRestoreTiming, error) {
	path := filepath.Join(f.RootDir, restoreTimingFile)
	var t childRestoreTiming
	data, err := os.ReadFile(path)
	if err != nil {
		return t, err
	}
	_ = os.Remove(path)
	err = json.Unmarshal(data, &t)
	return t, err
}

// bringLoopbackUp raises lo inside the fresh network namespace so restored
// processes keep their loopback connections.
func bringLoopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP | unix.IFF_RUNNING)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}
