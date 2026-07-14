package waypoint

// All criu(8) interactions: host compatibility checks, memory dumps, and the
// restore helper child that re-creates a fork's process tree inside fresh
// namespaces.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// --- compatibility ---

var criuCheckOnce sync.Once
var criuCheckErr error

// EnsureCriuCompatible verifies the criu binary on PATH can checkpoint and
// restore processes on this host. On aarch64 hosts with pointer
// authentication (PAC), CRIU older than 4.0 does not dump/restore the
// per-process PAC keys, so any restored process built with
// -mbranch-protection (bash, glibc, most distro userland) dies with SIGILL
// at the first authenticated return after restore.
func EnsureCriuCompatible() error {
	criuCheckOnce.Do(func() {
		criuCheckErr = checkCriuCompatible()
	})
	return criuCheckErr
}

func checkCriuCompatible() error {
	major, minor, err := criuVersion()
	if err != nil {
		return fmt.Errorf("criu not usable: %w", err)
	}
	if hostHasPAC() && major < 4 {
		return fmt.Errorf(
			"criu %d.%d cannot restore processes on this host: the CPU has ARM64 "+
				"pointer authentication (paca/pacg) and CRIU only checkpoints PAC keys "+
				"since 4.0; install criu >= 4.0", major, minor)
	}
	return nil
}

var criuVersionRe = regexp.MustCompile(`Version:\s*(\d+)\.(\d+)`)

func criuVersion() (int, int, error) {
	out, err := exec.Command("criu", "--version").CombinedOutput()
	if err != nil {
		return 0, 0, fmt.Errorf("criu --version failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	m := criuVersionRe.FindSubmatch(out)
	if m == nil {
		return 0, 0, fmt.Errorf("cannot parse criu --version output: %q", strings.TrimSpace(string(out)))
	}
	major, _ := strconv.Atoi(string(m[1]))
	minor, _ := strconv.Atoi(string(m[2]))
	return major, minor, nil
}

func hostHasPAC() bool {
	if runtime.GOARCH != "arm64" {
		return false
	}
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "Features") {
			continue
		}
		for _, f := range strings.Fields(line) {
			if f == "paca" || f == "pacg" {
				return true
			}
		}
	}
	return false
}

// --- dump ---

// createMemoryCheckpoint dumps a process tree with criu. The work overlay is
// declared as an external mount so CRIU treats it as managed by us.
// Notice: cannot use '--shell-job' because of the PTY issue during restore.
func (m *Manager) createMemoryCheckpoint(pid int, criuPath string) error {
	args := []string{"dump",
		"-t", strconv.Itoa(pid),
		"-D", criuPath,
		"--tcp-established",
		"--ghost-limit", "8388608",
		"-vv", "-o", "dump.log",
	}
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

func restoreForkChild(f *Fork) error {
	if err := prepareForkMountNamespace(f); err != nil {
		return err
	}
	if err := bringLoopbackUp(); err != nil {
		return err
	}

	args := []string{
		"restore",
		"--images-dir", f.CriuPath,
		"--tcp-established",
		"--restore-detached",
		"--pidfile", f.PidFile,
		// Absolute path: criu resolves relative -o against the images dir,
		// which is shared by all forks of a checkpoint and would make
		// concurrent restores clobber one another's logs.
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
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("criu restore failed: %w\n%s", err, stderr.String())
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pid, err := readPIDFile(f.PidFile); err == nil && pid > 0 {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("criu restore did not write pidfile %s", f.PidFile)
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
