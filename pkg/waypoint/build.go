package waypoint

// Environment building: buildah-based rootfs builds from a Dockerfile, and
// StartShell, which stages bash_init into the overlay and launches it in
// fresh namespaces.

import (
	"bytes"
	"crypto/sha1"
	"debug/elf"
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

	m.originalDir = originalDir

	// Initialize overlay environment on top of it
	workDir, err := m.InitEnvironment(originalDir)
	if err != nil {
		return "", 0, fmt.Errorf("failed to initialize overlay environment: %w", err)
	}

	// Launch bash_init in the background inside fresh namespaces
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

	// 5. Copy rootfs -> workspace. The sandbox /dev (device nodes, devpts,
	// shm) is assembled at session start by bash_init's mountDeviceRuntime.
	return reflinkCopy(rootfs, workspaceDir)
}

// reflinkCopy copies the contents of srcDir into dstDir with cp
// --reflink=auto: instant copy-on-write clones on reflink-capable
// filesystems (xfs, btrfs), silently degrading to a regular copy elsewhere.
func reflinkCopy(srcDir, dstDir string) error {
	out, err := exec.Command("cp", "-a", "--reflink=auto", srcDir+"/.", dstDir).CombinedOutput()
	if err != nil {
		return fmt.Errorf("copy %s -> %s failed: %w: %s", srcDir, dstDir, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// StageEnvironment snapshots srcDir into the session's private original/
// directory, which serves as the overlay lowerdir for the session's whole
// lifetime. Sessions never use the caller's directory directly: OverlayFS
// does not support a lower layer changing underneath a mounted overlay, so
// the source must be immune to later edits.
func (m *Manager) StageEnvironment(srcDir string) (string, error) {
	absDir, err := filepath.Abs(srcDir)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(absDir); err != nil {
		return "", fmt.Errorf("source directory not usable: %w", err)
	}
	originalDir := filepath.Join(m.baseDir, "original")
	if err := os.RemoveAll(originalDir); err != nil {
		return "", fmt.Errorf("failed to clean original directory: %w", err)
	}
	if err := os.MkdirAll(originalDir, 0o755); err != nil {
		return "", err
	}
	if err := reflinkCopy(absDir, originalDir); err != nil {
		return "", err
	}
	return originalDir, nil
}

// PrepareNetworkDeps seeds minimal name-resolution files into a session
// rootfs when the image ships none. Called by InitEnvironment against the
// merged overlay view, so both `init` and `build` sessions get it and the
// staged files land in the main fork's upper layer, never in the source.
func PrepareNetworkDeps(rootfs string) error {
	// DNS
	if err := copyIfBlank(rootfs, "/etc/resolv.conf"); err != nil {
		return err
	}

	// Minimal local files for name resolution; "waypoint" is the sandbox
	// hostname set by bash_init.
	const hosts = "" +
		"127.0.0.1 localhost waypoint\n" +
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

	return nil
}

// baseGuestEnv is the fixed, OCI-style environment every session starts with.
// It deliberately does not inherit the invoking user's: the guest environment
// is process state, so anything here is baked into every checkpoint and fork
// of the session, and inheriting would make guest behavior depend on whoever
// happened to run `waypoint init`.
var baseGuestEnv = []string{
	"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	"HOME=/root",
	"TERM=xterm",
	"LANG=C.UTF-8",
}

// unattendedGuestEnv keeps interactive tooling from stalling a session that
// has nobody on the other end. A fork's shell is driven over a socket, so a
// pager waiting for a keypress or apt waiting for a confirmation does not
// merely look wrong — it hangs the command until the exec timeout.
//
// These belong in the session's fixed environment rather than in bash_init's
// inherited one: they are part of what a checkpoint captures, so every fork
// and every recursive snapshot must see exactly the same set.
var unattendedGuestEnv = []string{
	// Pagers: stream output directly instead of waiting for navigation input.
	"PAGER=cat",
	"GIT_PAGER=cat",
	"SYSTEMD_PAGER=cat",

	// Editors: return immediately instead of opening an interactive editor.
	"EDITOR=true",
	"VISUAL=true",
	"GIT_EDITOR=true",
	"GIT_SEQUENCE_EDITOR=true",

	// Git authentication: fail instead of prompting; accept new SSH host keys.
	"GIT_TERMINAL_PROMPT=0",
	"GIT_ASKPASS=/bin/true",
	"GIT_SSH_COMMAND=ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new",

	// Debian tools: disable prompts and report restarts without performing them.
	"DEBIAN_FRONTEND=noninteractive",
	"NEEDRESTART_MODE=l",

	// opam 2.0 uses OPAMYES; newer releases prefer OPAMCONFIRMLEVEL.
	"OPAMYES=1",
	"OPAMCONFIRMLEVEL=unsafe-yes",

	// Python/pip: emit output promptly and disable prompts or dynamic UI.
	"PYTHONUNBUFFERED=1",
	"PIP_NO_INPUT=1",
	"PIP_DISABLE_PIP_VERSION_CHECK=1",
	"PIP_PROGRESS_BAR=off",
}

// waypointPlumbingEnv is bash_init's own configuration. bash_init strips
// every WAYPOINT_* variable before handing the environment to the shell, so
// these never reach the guest.
var waypointPlumbingEnv = []string{
	"WAYPOINT_NAMESPACED=1",
	"WAYPOINT_REEXEC_PATH=/.waypoint/bash_init",
}

// sessionEnv assembles the environment bash_init is launched with.
func sessionEnv() []string {
	env := make([]string, 0, len(baseGuestEnv)+len(unattendedGuestEnv)+len(waypointPlumbingEnv))
	env = append(env, baseGuestEnv...)
	env = append(env, unattendedGuestEnv...)
	return append(env, waypointPlumbingEnv...)
}

// StartShell launches bash_init in fresh namespaces, pivoted into workDir.
// On success, it updates the session info with the shell PID and socket path for later use.
func (m *Manager) StartShell(workDir string) (int, string, error) {
	// Locate bash_init binary and stage it inside the overlay
	bashInitSrc := DefaultBashInitSrc
	if _, err := os.Stat(bashInitSrc); os.IsNotExist(err) {
		return ShellNotEnabled, "", fmt.Errorf("bash_init binary not found at %s", bashInitSrc)
	}
	// bash_init re-execs from inside the session rootfs, where host
	// libraries are unavailable, so it must be statically linked.
	if err := requireStaticBinary(bashInitSrc); err != nil {
		return ShellNotEnabled, "", err
	}
	if err := copyFile(bashInitSrc, filepath.Join(workDir, ".waypoint", "bash_init")); err != nil {
		return ShellNotEnabled, "", fmt.Errorf("failed to stage bash_init in session root: %w", err)
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
	// The rootfs must ship bash's own libraries; nothing is healed from the
	// host. A rootfs missing one fails the startup handshake, and the shell
	// log surfaced below carries the loader's error naming the library.

	cmd := exec.Command(bashInitSrc, canonicalSocketPath, workDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: uintptr(unix.CLONE_NEWPID | unix.CLONE_NEWNS | unix.CLONE_NEWNET | unix.CLONE_NEWIPC | unix.CLONE_NEWUTS),
		Setsid:     true, // new session = no controlling TTY
	}
	// Sessions get a fixed, OCI-style environment instead of inheriting the
	// invoking user's; see sessionEnv and the vars it assembles.
	cmd.Env = sessionEnv()

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
	m.shellStartTime, _ = procStartTime(m.shellPid) // 0 on failure = unverified kill
	m.shellSocket = socketPathThroughProcRoot(m.shellPid, canonicalSocketPath)
	// Must outlast bash_init's 10s startup handshake: a shell that dies on
	// startup (e.g. a rootfs missing one of bash's libraries) is reported
	// through the shell log, which only carries the shell's own error output
	// once the handshake has given up.
	if err := waitForShellSocket(m.shellSocket, waitCh, logPath, 15*time.Second); err != nil {
		_ = cmd.Process.Kill()
		m.shellPid = ShellNotEnabled
		m.shellStartTime = 0
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

// requireStaticBinary rejects a dynamically linked bash_init (one with an
// ELF interpreter). Non-ELF or unreadable files pass; staging fails on
// those with clearer errors later.
func requireStaticBinary(path string) error {
	f, err := elf.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return fmt.Errorf("%s is dynamically linked and cannot re-exec inside arbitrary session rootfses; rebuild it statically: CGO_ENABLED=0 go build ./cmd/bash-init", path)
		}
	}
	return nil
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
