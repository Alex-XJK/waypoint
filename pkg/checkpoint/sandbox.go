package checkpoint

// Lightweight filesystem isolation using Linux namespaces
// Implementation inspired by https://github.com/binpash/try

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// Helper functions for building shell commands

// buildHostDirMountCommands creates mount commands to bind-mount host directories into the sandbox
func buildHostDirMountCommands(hostDirs []string, sandboxRoot string) string {
	var cmds strings.Builder
	for _, hostDir := range hostDirs {
		target := sandboxRoot + hostDir
		// Check if directory exists, bind mount it, then remount read-only
		cmds.WriteString(fmt.Sprintf(
			"[ -d %s ] && mount --bind %s '%s' 2>/dev/null && mount -o remount,bind,ro '%s' 2>/dev/null || true\n",
			hostDir, hostDir, target, target))
	}
	return cmds.String()
}

// buildDevSetupCommands creates commands to set up a minimal /dev filesystem
func buildDevSetupCommands(sandboxRoot string) string {
	return fmt.Sprintf(`# Minimal /dev setup
mount -t tmpfs -o nosuid,noexec,mode=755 tmpfs '%s/dev'
mkdir -p '%s/dev/pts' '%s/dev/shm'
mount -t devpts -o newinstance,ptmxmode=0666,mode=0620,gid=5 devpts '%s/dev/pts' 2>/dev/null || true
mknod -m 666 '%s/dev/null' c 1 3 2>/dev/null || true
mknod -m 666 '%s/dev/zero' c 1 5 2>/dev/null || true
mknod -m 666 '%s/dev/tty' c 5 0 2>/dev/null || true
mknod -m 666 '%s/dev/random' c 1 8 2>/dev/null || true
mknod -m 666 '%s/dev/urandom' c 1 9 2>/dev/null || true
`, sandboxRoot, sandboxRoot, sandboxRoot, sandboxRoot, sandboxRoot, sandboxRoot, sandboxRoot, sandboxRoot, sandboxRoot)
}

// buildCriuMountCommand creates a command to bind-mount the CRIU images directory
func buildCriuMountCommand(criuPath, sandboxRoot string) string {
	criuTarget := sandboxRoot + "/.criu"
	return fmt.Sprintf(
		"mkdir -p '%s' 2>/dev/null || true\n"+
			"mount --bind '%s' '%s' 2>/dev/null && mount -o remount,bind,ro '%s' 2>/dev/null || true\n",
		criuTarget, criuPath, criuTarget, criuTarget)
}

// buildFinalMountCommands creates commands to mount proc, sys, tmp, and run
// conflictDirs is a list of directories that should NOT be mounted (e.g., if original dir is in /tmp)
func buildFinalMountCommands(conflictDirs []string) string {
	cmds := "mount -t proc proc -o nosuid,nodev,noexec,hidepid=2 /proc || true; " +
		"mount -t sysfs sysfs /sys || true; "
	
	// Check if /tmp conflicts
	skipTmp := false
	skipRun := false
	for _, dir := range conflictDirs {
		if dir == "/tmp" {
			skipTmp = true
		}
		if dir == "/run" {
			skipRun = true
		}
	}
	
	if !skipTmp {
		cmds += "mount -t tmpfs tmpfs /tmp || true; "
	}
	if !skipRun {
		cmds += "mount -t tmpfs tmpfs /run || true"
	}
	
	return cmds
}

// getConflictingSystemDirs returns system directories that conflict with the original directory
func getConflictingSystemDirs(originalDir string) []string {
	var conflicts []string
	systemDirs := []string{"/tmp", "/run"}
	
	for _, sysDir := range systemDirs {
		// Check if originalDir is inside this system directory
		if strings.HasPrefix(originalDir, sysDir+"/") || originalDir == sysDir {
			conflicts = append(conflicts, sysDir)
		}
	}
	
	return conflicts
}

// escapeShellArg escapes a shell argument for safe use in shell commands
func escapeShellArg(arg string) string {
	// Escape single quotes by replacing ' with '"'"'
	return strings.ReplaceAll(arg, "'", "'\"'\"'")
}

// PrepareSandboxEnvironment creates the necessary directory structure for a sandbox
// It prepares the work directory to serve as the sandbox root
func PrepareSandboxEnvironment(workDir string) error {
	// Create essential directories that will be mounted in the sandbox
	requiredDirs := []string{
		"proc",
		"sys",
		"dev",
		"tmp",
		"run",
	}

	for _, dir := range requiredDirs {
		target := filepath.Join(workDir, ".sandbox", dir)
		if err := os.MkdirAll(target, 0755); err != nil {
			return fmt.Errorf("failed to create sandbox dir %s: %w", dir, err)
		}
	}

	return nil
}

// RestoreInSandbox wraps CRIU restore to run in a sandboxed environment.
// This uses unshare --root to set the work directory as the root filesystem
func RestoreInSandbox(criuPath, workDir string, originalCmd *exec.Cmd) (*exec.Cmd, error) {
	// Get absolute paths
	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path: %w", err)
	}

	absCriuPath, err := filepath.Abs(criuPath)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path for CRIU images: %w", err)
	}

	// Create directories in workDir for essential paths that will be mounted
	requiredDirs := []string{"bin", "lib", "lib64", "usr/lib", "usr/lib64", "usr/bin", "sbin", "usr/sbin", "dev", "proc", "sys", "tmp", "run"}
	for _, dir := range requiredDirs {
		target := filepath.Join(absWorkDir, dir)
		if err := os.MkdirAll(target, 0755); err != nil {
			return nil, fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	// Build mount setup commands
	var mountCmds strings.Builder
	mountCmds.WriteString("mount --make-rprivate /\n") // Privatize mount propagation
	
	// Bind mount host directories (read-only for safety)
	hostDirs := []string{"/lib", "/lib64", "/usr/lib", "/usr/lib64", "/bin", "/usr/bin", "/sbin", "/usr/sbin"}
	mountCmds.WriteString(buildHostDirMountCommands(hostDirs, absWorkDir))
	
	// Bind mount CRIU images directory
	mountCmds.WriteString(buildCriuMountCommand(absCriuPath, absWorkDir))
	
	// Setup minimal /dev filesystem
	mountCmds.WriteString(buildDevSetupCommands(absWorkDir))
	
	// Build the final command that runs inside the sandbox
	// Note: RestoreInSandbox doesn't need to worry about conflicts since CRIU images are in a separate location
	finalMounts := buildFinalMountCommands([]string{})
	criuRestoreCmd := "exec criu restore --images-dir /.criu --shell-job --tcp-established"
	
	// Build the complete setup script
	// Use pivot_root instead of unshare --root for better OverlayFS compatibility
	setupScript := fmt.Sprintf(`#!/bin/sh
# Set up bind mounts from host filesystem into workDir
# These mounts are in a new mount namespace, so they don't affect the parent
%s
# Use pivot_root instead of unshare --root for better OverlayFS compatibility
# Create a temporary directory for the old root
OLDROOT=$(mktemp -d -p '%s' .oldroot.XXXXXX) || exit 1
# Pivot to make workDir the new root
pivot_root '%s' "$OLDROOT" || exit 1
# Move old root out of the way and unmount it
cd /
umount "$OLDROOT" 2>/dev/null || true
rmdir "$OLDROOT" 2>/dev/null || true
# Mount /proc with hidepid=2 to prevent non-root from seeing host processes
# Mount /sys, /tmp, /run as tmpfs
%s
# Execute CRIU restore
%s
`, mountCmds.String(), absWorkDir, absWorkDir, finalMounts, criuRestoreCmd)

	// Use unshare to create mount and PID namespaces, then run setup script
	// The setup script will do bind mounts (isolated), then use unshare --root nested
	sandboxedCmd := exec.Command("unshare",
		"--mount",
		"--fork",
		"--pid",
		"sh", "-c", setupScript)

	// Preserve SysProcAttr from original command (needed for CRIU's root privileges)
	if originalCmd != nil && originalCmd.SysProcAttr != nil {
		sandboxedCmd.SysProcAttr = originalCmd.SysProcAttr
	} else {
		// CRIU requires root privileges
		sandboxedCmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{
				Uid: 0,
				Gid: 0,
			},
		}
	}

	return sandboxedCmd, nil
}

// copyNetworkConfig copies /etc/resolv.conf and /etc/hosts from host to workDir
// This is needed because buildah images don't include these files (Docker injects them at runtime)
func copyNetworkConfig(workDir string) error {
	etcDir := filepath.Join(workDir, "etc")
	if err := os.MkdirAll(etcDir, 0755); err != nil {
		return fmt.Errorf("failed to create /etc in sandbox: %w", err)
	}

	// Files to copy from host
	filesToCopy := []string{"resolv.conf", "hosts"}

	for _, filename := range filesToCopy {
		srcPath := filepath.Join("/etc", filename)
		dstPath := filepath.Join(etcDir, filename)

		// Read source file
		content, err := os.ReadFile(srcPath)
		if err != nil {
			// Skip if file doesn't exist on host
			continue
		}

		// Write to destination (overwrite if exists)
		if err := os.WriteFile(dstPath, content, 0644); err != nil {
			return fmt.Errorf("failed to copy %s to sandbox: %w", filename, err)
		}
	}

	return nil
}

// ExecuteInSandbox creates a command that runs an arbitrary command in a fully isolated sandbox.
// The sandbox is Docker-like: workDir becomes the root filesystem (/), and no host directories are mounted.
// This assumes workDir already contains a complete filesystem (copied from Docker) with /bin, /usr, etc.
// originalDir is ignored - the command always starts in / (the root of workDir).
func ExecuteInSandbox(workDir, originalDir string, command string, args ...string) (*exec.Cmd, error) {
	// Get absolute path for workDir
	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path: %w", err)
	}

	// Ensure essential directories exist in workDir
	// These should already exist if copied from Docker, but we create them just in case
	requiredDirs := []string{"dev", "proc", "sys", "tmp", "run"}
	for _, dir := range requiredDirs {
		target := filepath.Join(absWorkDir, dir)
		if err := os.MkdirAll(target, 0755); err != nil {
			return nil, fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	// Copy network configuration (DNS) from host to sandbox
	// This is needed because buildah images don't include /etc/resolv.conf
	if err := copyNetworkConfig(absWorkDir); err != nil {
		// Log warning but continue - network may not be needed
		fmt.Fprintf(os.Stderr, "Warning: failed to copy network config to sandbox: %v\n", err)
	}

	// Build command with escaped arguments
	cmdParts := make([]string, 0, len(args)+1)
	cmdParts = append(cmdParts, command)
	for _, arg := range args {
		escaped := escapeShellArg(arg)
		cmdParts = append(cmdParts, fmt.Sprintf("'%s'", escaped))
	}
	execCmd := strings.Join(cmdParts, " ")
	
	// Build the complete setup script
	// Docker-like isolation: workDir becomes / and nothing from host is visible
	setupScript := fmt.Sprintf(`#!/bin/sh
# Privatize mount propagation to isolate from host
mount --make-rprivate / 2>/dev/null || true

# Setup minimal /dev filesystem
mount -t tmpfs -o nosuid,noexec,mode=755 tmpfs '%s/dev' 2>/dev/null || true
mkdir -p '%s/dev/pts' '%s/dev/shm' 2>/dev/null || true
mount -t devpts -o newinstance,ptmxmode=0666,mode=0620,gid=5 devpts '%s/dev/pts' 2>/dev/null || true
mknod -m 666 '%s/dev/null' c 1 3 2>/dev/null || true
mknod -m 666 '%s/dev/zero' c 1 5 2>/dev/null || true
mknod -m 666 '%s/dev/tty' c 5 0 2>/dev/null || true
mknod -m 666 '%s/dev/random' c 1 8 2>/dev/null || true
mknod -m 666 '%s/dev/urandom' c 1 9 2>/dev/null || true

# Use pivot_root to make workDir the new root filesystem
# Create a temporary directory for the old root
OLDROOT=$(mktemp -d -p '%s' .oldroot.XXXXXX) || exit 1

# Pivot to make workDir the new root
pivot_root '%s' "$OLDROOT" || exit 1

# Move to new root and clean up old root
cd /
umount -l "$OLDROOT" 2>/dev/null || true
rmdir "$OLDROOT" 2>/dev/null || true

# Mount virtual filesystems (these should be in the new root now)
mount -t proc proc -o nosuid,nodev,noexec,hidepid=2 /proc 2>/dev/null || true
mount -t sysfs sysfs /sys 2>/dev/null || true
mount -t tmpfs tmpfs /tmp 2>/dev/null || true
mount -t tmpfs tmpfs /run 2>/dev/null || true

# Execute the command in / (which is the workDir content)
cd /
exec %s
`, absWorkDir, absWorkDir, absWorkDir, absWorkDir, absWorkDir, absWorkDir, absWorkDir, absWorkDir, absWorkDir, absWorkDir, absWorkDir, execCmd)

	// Use unshare to create isolated namespaces (mount, PID, network, IPC, UTS)
	// This creates a Docker-like container environment
	sandboxedCmd := exec.Command("unshare",
		"--mount",
		"--fork",
		"--pid",
		"sh", "-c", setupScript)

	// Run as root (required for mount operations and namespace creation)
	sandboxedCmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid: 0,
			Gid: 0,
		},
	}

	return sandboxedCmd, nil
}
