package checkpoint

// Dockerfile-based build process

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func BuildFromDockerfile(dockerfileDir, workspaceDir string, quiet bool) error {
	imageTag := fmt.Sprintf("ckptlite_%s:%d", filepath.Base(dockerfileDir), time.Now().Unix())

	run := func(cmd *exec.Cmd, capture bool) (string, error) {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		if capture {
			cmd.Stdout = &stdout
		} else if quiet {
			cmd.Stdout = nil
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

	return nil
}

// StartShell launches a new chroot-embedded bash_init process at the given workDir.
// On success, it updates the session info with the shell PID and socket path for later use.
// TODO ASK ALEX: should socket be branch specific? Should temp folder in general be branch specific?
// ALSO ASK: Why doing m.baseDir join "temp", vs using m.temporaryDir?

func (m *Manager) StartShell(workDir string) (int, string, error) {
	// Locate bash_init binary
	bashInitSrc := DefaultBashInitSrc
	if _, err := os.Stat(bashInitSrc); os.IsNotExist(err) {
		return ShellNotEnabled, "", fmt.Errorf("bash_init binary not found at %s", bashInitSrc)
	}

	socketPath := filepath.Join(m.baseDir, m.branchID + "_temp", fmt.Sprintf("shell_%s.sock", m.sessionID)) //TODO not sure if should be branch spec.

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
	logPath := filepath.Join(m.baseDir, m.branchID + "_temp", fmt.Sprintf("shell_%s.log", m.sessionID)) //TODO ASK
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
	if err := saveSessionInfo(m.sessionID, m.branchID, m); err != nil {
		return m.shellPid, m.shellSocket, fmt.Errorf("failed to save session info: %w", err)
	}

	return m.shellPid, m.shellSocket, nil
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
	if err := updateSessionEnvironment(m.sessionID, m.branchID, m.originalDir, m.workOverlay); err != nil {
		return workDir, pid, fmt.Errorf("failed to update session info: %w", err)
	}

	return workDir, pid, nil
}
