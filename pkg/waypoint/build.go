package waypoint

// Dockerfile-based build process

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
	devDir := filepath.Join(workspaceDir, "dev")
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

	type devSpec struct {
		name  string
		major uint32
		minor uint32
		perm  os.FileMode
	}
	devices := []devSpec{
		{"null", 1, 3, 0o666},
		{"zero", 1, 5, 0o666},
		{"random", 1, 8, 0o666},
		{"urandom", 1, 9, 0o666},
	}

	// Helper to (re)create a char device with given major/minor
	makeChar := func(path string, major, minor uint32, perm os.FileMode) error {
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

	for _, d := range devices {
		p := filepath.Join(devDir, d.name)
		if err := makeChar(p, d.major, d.minor, d.perm); err != nil {
			return err
		}
	}

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
	// Locate bash_init binary
	bashInitSrc := DefaultBashInitSrc
	if _, err := os.Stat(bashInitSrc); os.IsNotExist(err) {
		return ShellNotEnabled, "", fmt.Errorf("bash_init binary not found at %s", bashInitSrc)
	}

	socketPath := filepath.Join(m.baseDir, "temp", fmt.Sprintf("shell_%s.sock", m.sessionID))

	// Judge /bin/bash pre-requisite for bash_init
	bashPath := filepath.Join(workDir, "bin/bash")
	if _, err := os.Stat(bashPath); os.IsNotExist(err) {
		return ShellNotEnabled, "", fmt.Errorf("bash pre-requisite not met: %s does not exist", bashPath)
	}

	cmd := exec.Command(bashInitSrc, socketPath, workDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, // new session = no controlling TTY
	}

	// stdin -> /dev/null
	devNull, err := os.OpenFile("/dev/null", os.O_RDWR, 0)
	if err != nil {
		return ShellNotEnabled, "", fmt.Errorf("failed to open /dev/null: %w", err)
	}
	cmd.Stdin = devNull

	// stdout/stderr -> log file
	logPath := filepath.Join(m.baseDir, "temp", fmt.Sprintf("shell_%s.log", m.sessionID))
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return ShellNotEnabled, "", fmt.Errorf("failed to open log file: %w", err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	// Start the bash_init process in the background
	if err := cmd.Start(); err != nil {
		return ShellNotEnabled, "", fmt.Errorf("failed to start bash_init: %w", err)
	}

	// Update shell PID and socket path in session info
	m.shellPid = cmd.Process.Pid
	m.shellSocket = socketPath

	// Save updated session info
	if err := saveSessionInfo(m.sessionID, m); err != nil {
		return m.shellPid, m.shellSocket, fmt.Errorf("failed to save session info: %w", err)
	}

	return m.shellPid, m.shellSocket, nil
}

func ensureBinAndDeps(rootfs, bin string) error {
	if err := copyIfBlank(rootfs, bin); err != nil {
		return err
	}

	deps, err := lddPaths(bin)
	if err != nil {
		return err
	}
	for _, dep := range deps {
		if err := copyIfBlank(rootfs, dep); err != nil {
			return err
		}
	}
	return nil
}

func lddPaths(bin string) ([]string, error) {
	out, err := exec.Command("ldd", bin).Output()
	if err != nil {
		return nil, err
	}

	var deps []string
	seen := map[string]bool{}

	s := bufio.NewScanner(bytes.NewReader(out))
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
	buildahErr := BuildFromDockerfile(dockerfileDir, originalDir, quiet)
	if buildahErr != nil {
		return "", 0, fmt.Errorf("failed to build from Dockerfile: %w", buildahErr)
	}
	if pndErr := PrepareNetworkDeps(originalDir); pndErr != nil {
		return "", 0, fmt.Errorf("failed to prepare network: %w", pndErr)
	}

	// Now that we have a built environment ready.
	m.originalDir = originalDir

	// Initialize overlay environment on top of it
	workDir, overlayErr := m.InitEnvironment(originalDir)
	if overlayErr != nil {
		return "", 0, fmt.Errorf("failed to initialize overlay environment: %w", overlayErr)
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
