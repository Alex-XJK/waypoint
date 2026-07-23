package waypoint

// Environment building: buildah-based rootfs builds from a Dockerfile, and
// StartShell, which stages bash_init into the overlay and launches it in
// fresh namespaces. Includes the ldd-based dependency-staging helpers.

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// BuildEnvironment builds a rootfs from a Dockerfile, mounts the session
// overlay on top of it, and starts the shell inside.
func (m *Manager) BuildEnvironment(dockerfileDir string, quiet bool) (string, int, error) {
	originalDir := filepath.Join(m.baseDir, "original")

	// Ensure originalDir is clean
	if err := os.RemoveAll(originalDir); err != nil {
		return "", 0, fmt.Errorf("failed to clean original directory: %w", err)
	}
	if err := os.MkdirAll(originalDir, 0755); err != nil {
		return "", 0, fmt.Errorf("failed to create original directory: %w", err)
	}

	// Build from Dockerfile to create a virtual system environment
	if err := BuildFromDockerfile(dockerfileDir, originalDir, quiet); err != nil {
		return "", 0, fmt.Errorf("failed to build from Dockerfile: %w", err)
	}
	if err := PrepareNetworkDeps(originalDir); err != nil {
		return "", 0, fmt.Errorf("failed to prepare network: %w", err)
	}

	m.originalDir = originalDir

	// Initialize overlay environment on top of it
	workDir, err := m.InitEnvironment(originalDir)
	if err != nil {
		return "", 0, fmt.Errorf("failed to initialize overlay environment: %w", err)
	}

	// Launch new chroot-embedded bash_init in background to set up the environment
	pid, _, err := m.StartShell(workDir)
	if err != nil {
		return workDir, pid, fmt.Errorf("failed to start shell in environment: %w", err)
	}

	// Update session info with originalDir, workOverlay, shell PID, and socket path
	if err := updateSessionEnvironment(m.sessionID, m.originalDir, m.workOverlay); err != nil {
		return workDir, pid, fmt.Errorf("failed to update session info: %w", err)
	}

	return workDir, pid, nil
}

// imageRefComponent sanitizes s (a build-context directory basename) into a
// string usable inside a buildah/Docker image reference. Reference name
// components must be lowercase and match
// [a-z0-9]+((?:[._]|__|[-]+)[a-z0-9]+)*, so they may not start or end with a
// separator, nor contain other characters — a trailing "_" or an uppercase
// letter (e.g. a tempfile.mkdtemp dir like "img3_4l96kk1_") otherwise yields
// "invalid reference format". We lowercase, collapse every run of disallowed
// characters into a single "-", and trim trailing separators. If nothing
// usable remains, we fall back to a short hash so the result is always a
// valid, deterministic component.
func imageRefComponent(s string) string {
	var b strings.Builder
	sep := true // start "after a separator" so leading junk is dropped
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			sep = false
			continue
		}
		if !sep {
			b.WriteByte('-')
			sep = true
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if out == "" {
		sum := sha1.Sum([]byte(s))
		return hex.EncodeToString(sum[:])[:12]
	}
	return out
}

func BuildFromDockerfile(dockerfileDir, workspaceDir string, quiet bool) error {
	imageTag := fmt.Sprintf("waypoint_%s:%d", imageRefComponent(filepath.Base(dockerfileDir)), time.Now().Unix())

	run := func(cmd *exec.Cmd, capture bool) (string, error) {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		if capture {
			cmd.Stdout = &stdout
		} else if quiet {
			cmd.Stdout = io.Discard
		} else {
			cmd.Stdout = os.Stdout
		}
		cmd.Stderr = &stderr

		if err := cmd.Run(); err != nil {
			return stdout.String(),
				fmt.Errorf("command failed: %s\nstderr: %s",
					strings.Join(cmd.Args, " "),
					stderr.String())
		}
		return strings.TrimSpace(stdout.String()), nil
	}

	// 1. buildah bud
	if _, err := run(exec.Command(
		"buildah", "bud", "-t", imageTag, "-f", filepath.Join(dockerfileDir, "Dockerfile"), dockerfileDir,
	), false); err != nil {
		return err
	}

	// 2. buildah from -q
	cid, err := run(exec.Command("buildah", "from", "-q", imageTag), true)
	if err != nil {
		return err
	}
	if cid == "" {
		return fmt.Errorf("buildah from did not return a container id")
	}

	// Ensure cleanup
	defer func() {
		_, _ = run(exec.Command("buildah", "unmount", cid), false)
		_, _ = run(exec.Command("buildah", "rm", cid), false)
	}()

	// 3. buildah mount
	rootfs, err := run(exec.Command("buildah", "mount", cid), true)
	if err != nil {
		return err
	}
	if rootfs == "" {
		return fmt.Errorf("buildah mount did not return rootfs path")
	}

	// 4. Clean workspace
	if err := os.RemoveAll(workspaceDir); err != nil {
		return err
	}
	if err := os.MkdirAll(workspaceDir, 0755); err != nil {
		return err
	}

	// 5. Copy rootfs -> workspace
	if _, err := run(exec.Command(
		"rsync", "-a",
		rootfs+"/",
		workspaceDir,
	), false); err != nil {
		if _, err := run(exec.Command(
			"bash", "-lc",
			fmt.Sprintf("cp -a '%s/.' '%s'", rootfs, workspaceDir),
		), false); err != nil {
			return fmt.Errorf("failed to copy rootfs: %w", err)
		}
	}

	// 6. Ensure basic char devices exist
	return prepareDevNodes(filepath.Join(workspaceDir, "dev"))
}

// prepareDevNodes creates the minimal /dev the environment needs: a
// world-writable sticky /dev/shm and the basic character devices.
func prepareDevNodes(devDir string) error {
	if err := os.MkdirAll(devDir, 0755); err != nil {
		return fmt.Errorf("failed to create dev directory: %w", err)
	}

	// Create a mimic /dev/shm with 0x1777
	shmDir := filepath.Join(devDir, "shm")
	if fi, err := os.Lstat(shmDir); err == nil {
		if !fi.IsDir() {
			if rmErr := os.Remove(shmDir); rmErr != nil {
				return fmt.Errorf("failed to remove existing %s: %w", shmDir, rmErr)
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("failed to stat %s: %w", shmDir, err)
	}
	if err := os.MkdirAll(shmDir, 0o1777); err != nil {
		return fmt.Errorf("failed to create shm directory: %w", err)
	}
	if err := os.Chmod(shmDir, 0o1777); err != nil {
		return fmt.Errorf("failed to chmod %s: %w", shmDir, err)
	}
	_ = os.Chown(shmDir, 0, 0)

	devices := []struct {
		name         string
		major, minor uint32
		perm         os.FileMode
	}{
		{"null", 1, 3, 0o666},
		{"zero", 1, 5, 0o666},
		{"random", 1, 8, 0o666},
		{"urandom", 1, 9, 0o666},
	}
	for _, d := range devices {
		if err := makeCharDevice(filepath.Join(devDir, d.name), d.major, d.minor, d.perm); err != nil {
			return err
		}
	}
	return nil
}

// makeCharDevice (re)creates a character device node with the given major/minor.
func makeCharDevice(path string, major, minor uint32, perm os.FileMode) error {
	// Remove existing non-char file
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeDevice == 0 || fi.Mode()&os.ModeCharDevice == 0 {
			if rmErr := os.Remove(path); rmErr != nil {
				return fmt.Errorf("failed to remove existing %s: %w", path, rmErr)
			}
		}
	}
	// Create node if missing
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		dev := unix.Mkdev(major, minor)
		mode := uint32(unix.S_IFCHR | uint32(perm&0o777))
		if err := unix.Mknod(path, mode, int(dev)); err != nil {
			return fmt.Errorf("mknod %s failed (major=%d minor=%d): %w", path, major, minor, err)
		}
	}
	// Ensure permissions are as requested (umask-safe)
	if err := os.Chmod(path, perm); err != nil {
		return fmt.Errorf("chmod %s failed: %w", path, err)
	}
	// Ensure ownership root:root (best-effort)
	_ = os.Chown(path, 0, 0)
	return nil
}

func PrepareNetworkDeps(rootfs string) error {
	// DNS
	if err := copyIfBlank(rootfs, "/etc/resolv.conf"); err != nil {
		return err
	}

	// Minimal local files for name resolution
	const hosts = "" +
		"127.0.0.1 localhost\n" +
		"::1 localhost ip6-localhost ip6-loopback\n"
	if err := writeIfBlank(filepath.Join(rootfs, "/etc/hosts"), []byte(hosts), 0o644); err != nil {
		return err
	}

	const nsswitch = "" +
		"passwd: files\n" +
		"group: files\n" +
		"shadow: files\n" +
		"hosts: files dns\n"
	if err := writeIfBlank(filepath.Join(rootfs, "/etc/nsswitch.conf"), []byte(nsswitch), 0o644); err != nil {
		return err
	}

	// APT signature verification
	if err := ensureBinAndDeps(rootfs, "/usr/bin/gpgv"); err != nil {
		return err
	}

	_ = copyIfBlank(rootfs, "/usr/share/keyrings/ubuntu-archive-keyring.gpg")
	_ = copyIfBlank(rootfs, "/usr/share/keyrings/ubuntu-archive-removed-keys.gpg")
	_ = copyIfBlank(rootfs, "/etc/apt/trusted.gpg.d/ubuntu-keyring-2018-archive.gpg")
	_ = copyIfBlank(rootfs, "/etc/apt/trusted.gpg.d/ubuntu-keyring-2012-cdimage.gpg")

	return nil
}

// StartShell launches a new chroot-embedded bash_init process at the given workDir.
// On success, it updates the session info with the shell PID and socket path for later use.
func (m *Manager) StartShell(workDir string) (int, string, error) {
	// Locate bash_init binary and stage it inside the overlay
	bashInitSrc := DefaultBashInitSrc
	if _, err := os.Stat(bashInitSrc); os.IsNotExist(err) {
		return ShellNotEnabled, "", fmt.Errorf("bash_init binary not found at %s", bashInitSrc)
	}
	if err := copyFile(bashInitSrc, filepath.Join(workDir, ".waypoint", "bash_init")); err != nil {
		return ShellNotEnabled, "", fmt.Errorf("failed to stage bash_init in session root: %w", err)
	}
	if err := stageRuntimeDeps(workDir, bashInitSrc); err != nil {
		return ShellNotEnabled, "", fmt.Errorf("failed to stage bash_init runtime dependencies: %w", err)
	}

	canonicalSocketPath := m.canonicalSocketPath()
	hostSocketPath := filepath.Join(workDir, strings.TrimPrefix(canonicalSocketPath, string(filepath.Separator)))
	if err := os.MkdirAll(filepath.Dir(hostSocketPath), 0o777); err != nil {
		return ShellNotEnabled, "", fmt.Errorf("failed to create shell socket directory: %w", err)
	}
	logPath := m.shellLogPath()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return ShellNotEnabled, "", fmt.Errorf("failed to create shell log directory: %w", err)
	}

	// Judge /bin/bash pre-requisite for bash_init
	bashPath := filepath.Join(workDir, "bin/bash")
	if _, err := os.Stat(bashPath); os.IsNotExist(err) {
		return ShellNotEnabled, "", fmt.Errorf("bash pre-requisite not met: %s does not exist", bashPath)
	}
	// A rootfs with bash but without its shared libraries (libtinfo etc.)
	// produces a shell that dies after startup; heal from the host when the
	// libraries resolve there.
	if err := stageRuntimeDeps(workDir, bashPath); err != nil {
		return ShellNotEnabled, "", fmt.Errorf("failed to stage bash runtime dependencies: %w", err)
	}

	cmd := exec.Command(bashInitSrc, canonicalSocketPath, workDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: uintptr(unix.CLONE_NEWPID | unix.CLONE_NEWNS | unix.CLONE_NEWNET | unix.CLONE_NEWIPC),
		Setsid:     true, // new session = no controlling TTY
	}
	cmd.Env = append(os.Environ(),
		"WAYPOINT_NAMESPACED=1",
		"WAYPOINT_REEXEC_PATH=/.waypoint/bash_init",
	)

	// stdin -> /dev/null
	devNull, err := os.OpenFile("/dev/null", os.O_RDWR, 0)
	if err != nil {
		return ShellNotEnabled, "", fmt.Errorf("failed to open /dev/null: %w", err)
	}
	defer devNull.Close()
	cmd.Stdin = devNull

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return ShellNotEnabled, "", fmt.Errorf("failed to open shell startup log: %w", err)
	}
	defer logFile.Close()
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	// Start the bash_init process in the background
	if err := cmd.Start(); err != nil {
		return ShellNotEnabled, "", fmt.Errorf("failed to start bash_init: %w", err)
	}
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	// Update shell PID and socket path in session info
	m.shellPid = cmd.Process.Pid
	m.shellSocket = socketPathThroughProcRoot(m.shellPid, canonicalSocketPath)
	if err := waitForShellSocket(m.shellSocket, waitCh, logPath, 5*time.Second); err != nil {
		_ = cmd.Process.Kill()
		m.shellPid = ShellNotEnabled
		m.shellSocket = ""
		return ShellNotEnabled, "", err
	}
	if err := m.saveMainFork(m.shellPid, m.shellSocket, canonicalSocketPath, logPath); err != nil {
		return m.shellPid, m.shellSocket, fmt.Errorf("failed to save main fork: %w", err)
	}

	// Save updated session info
	if err := saveSessionInfo(m.sessionID, m); err != nil {
		return m.shellPid, m.shellSocket, fmt.Errorf("failed to save session info: %w", err)
	}

	return m.shellPid, m.shellSocket, nil
}

func waitForShellSocket(path string, waitCh <-chan error, logPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		select {
		case err := <-waitCh:
			if err == nil {
				err = fmt.Errorf("bash_init exited")
			}
			return fmt.Errorf("bash_init exited before shell socket became ready: %w\nstartup log %s:\n%s", err, logPath, readRecentFile(logPath, 16*1024))
		default:
		}

		if err := dialUnixSocket(path, 100*time.Millisecond); err == nil {
			return nil
		} else {
			lastErr = err
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for shell socket %s: %v\nstartup log %s:\n%s", path, lastErr, logPath, readRecentFile(logPath, 16*1024))
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func readRecentFile(path string, maxBytes int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(unable to read log: %v)", err)
	}
	if len(data) > maxBytes {
		data = data[len(data)-maxBytes:]
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return "(empty)"
	}
	return string(data)
}

// --- dependency staging (ldd -> copy libs into the rootfs) ---

// stageRuntimeDeps copies host-resolved shared-library dependencies of a
// binary into the rootfs where they are missing. The binary may be a host
// path or live inside the rootfs.
func stageRuntimeDeps(rootfs, binaryPath string) error {
	deps, err := lddPaths(binaryPath)
	if err != nil {
		return fmt.Errorf("failed to inspect %s runtime dependencies: %w", binaryPath, err)
	}
	for _, dep := range deps {
		if err := copyIfBlank(rootfs, dep); err != nil {
			return fmt.Errorf("failed to stage dependency %s for %s: %w", dep, binaryPath, err)
		}
	}
	return nil
}

func ensureBinAndDeps(rootfs, bin string) error {
	if err := copyIfBlank(rootfs, bin); err != nil {
		return err
	}
	return stageRuntimeDeps(rootfs, bin)
}

func lddPaths(bin string) ([]string, error) {
	out, err := exec.Command("ldd", bin).CombinedOutput()
	text := string(out)
	if err != nil {
		if strings.Contains(text, "not a dynamic executable") || strings.Contains(text, "statically linked") {
			return nil, nil
		}
		return nil, fmt.Errorf("ldd %s failed: %w: %s", bin, err, strings.TrimSpace(text))
	}
	if strings.Contains(text, "statically linked") {
		return nil, nil
	}

	var deps []string
	seen := map[string]bool{}

	s := bufio.NewScanner(strings.NewReader(text))
	for s.Scan() {
		line := s.Text()

		if strings.Contains(line, "not found") {
			return nil, fmt.Errorf("ldd missing dependency: %s", strings.TrimSpace(line))
		}

		for _, f := range strings.Fields(line) {
			if strings.HasPrefix(f, "/") && !seen[f] {
				seen[f] = true
				deps = append(deps, f)
				break
			}
		}
	}
	return deps, s.Err()
}

// copyIfBlank copies a host file to the same path inside the rootfs if the
// destination is missing or empty.
func copyIfBlank(rootfs, hostAbs string) error {
	if _, err := os.Stat(hostAbs); err != nil {
		return nil
	}
	dst := filepath.Join(rootfs, hostAbs)
	if !isMissingOrBlank(dst) {
		return nil
	}
	return copyFile(hostAbs, dst)
}

func writeIfBlank(dst string, data []byte, mode os.FileMode) error {
	if !isMissingOrBlank(dst) {
		return nil
	}
	_ = os.MkdirAll(filepath.Dir(dst), 0o755)
	return os.WriteFile(dst, data, mode)
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}

	st, err := os.Stat(src)
	if err != nil {
		return err
	}

	_ = os.MkdirAll(filepath.Dir(dst), 0o755)
	return os.WriteFile(dst, data, st.Mode().Perm())
}

func isMissingOrBlank(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	return len(bytes.TrimSpace(data)) == 0
}
