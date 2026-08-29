package waypoint

// File transfer between the host and a live fork.
//
// A fork's filesystem is a private OverlayFS mounted inside its own mount
// namespace, so a host-side path prefix like /proc/<pid>/root/<path> is NOT a
// safe target: an absolute symlink inside the fork (e.g. /work/x -> /tmp/y)
// resolves /tmp against the HOST there, silently escaping the fork. So every
// copy runs from a child that has joined the fork's namespaces, where the fork
// path resolves exactly as the fork sees it. The host end is a pipe fd
// inherited across the namespace join (setns does not close fds), so bytes
// stream with no size limit — well past the exec protocol's 1 MiB request cap.

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// CopyDirection is the transfer direction relative to the fork.
type CopyDirection int

const (
	// CopyIn writes from the host into the fork.
	CopyIn CopyDirection = iota
	// CopyOut reads from the fork out to the host.
	CopyOut
)

// Copy transfers a file or directory between the host and a live fork,
// serialized against exec/snapshot/destroy on the same fork by the per-fork
// lock (so a snapshot cannot swap the fork's pid out mid-copy). forkPath is
// always the in-fork path and hostPath always the host path, regardless of
// direction. Directories are transferred recursively, preserving mode, mtime
// and symlinks (cp -a semantics); a single file into a nonexistent path is a
// rename. Parent directories on the destination side are created.
func (m *Manager) Copy(forkID, forkPath, hostPath string, dir CopyDirection) error {
	return m.withForkLock(forkID, func() error {
		f, err := m.loadFork(forkID)
		if err != nil {
			return fmt.Errorf("fork %s not found (it may have been parked or destroyed; fork a checkpoint to get a live copy)", forkID)
		}
		if f.Status != ForkStatusRunning {
			return fmt.Errorf("fork %s is not running (status=%s); fork a checkpoint to get a live copy", forkID, f.Status)
		}
		// PID-reuse guard: the record's pid must still be the shell we forked,
		// not a recycled pid now owned by something else. Mirrors killTree's
		// start-time check.
		if f.StartTime != 0 {
			cur, err := procStartTime(f.PID)
			if err != nil {
				return fmt.Errorf("fork %s pid %d is gone: %w", forkID, f.PID, err)
			}
			if cur != f.StartTime {
				return fmt.Errorf("fork %s pid %d was reused (start time %d != %d); refusing to copy", forkID, f.PID, cur, f.StartTime)
			}
		}
		switch dir {
		case CopyIn:
			return copyIn(f.PID, hostPath, forkPath)
		case CopyOut:
			return copyOut(f.PID, forkPath, hostPath)
		default:
			return fmt.Errorf("unknown copy direction %d", dir)
		}
	})
}

func copyIn(pid int, hostSrc, forkDst string) error {
	info, err := os.Stat(hostSrc)
	if err != nil {
		return fmt.Errorf("stat host source %s: %w", hostSrc, err)
	}
	if info.IsDir() {
		return copyDirIn(pid, hostSrc, forkDst)
	}
	return copyFileIn(pid, hostSrc, forkDst)
}

// copyFileIn streams a host file into the fork. The host file is opened in the
// host mount namespace (before the join) and becomes the child's stdin; the
// child, inside the fork, writes the fork path.
func copyFileIn(pid int, hostSrc, forkDst string) error {
	src, err := os.Open(hostSrc)
	if err != nil {
		return fmt.Errorf("open host source %s: %w", hostSrc, err)
	}
	defer src.Close()

	script := fmt.Sprintf(`mkdir -p -- "$(dirname -- %s)" && cat > %s`,
		shellQuote(forkDst), shellQuote(forkDst))
	cmd := forkNsCommand(pid, "/bin/sh", "-c", script)
	cmd.Stdin = src
	return runNsChild(cmd, "copy file into fork")
}

// copyDirIn streams a host directory tree into the fork over tar, preserving
// attributes. tar on the host side (host mount ns) produces the stream; the
// child, inside the fork, extracts it under the destination.
func copyDirIn(pid int, hostSrc, forkDst string) error {
	producer := exec.Command("tar", "-C", hostSrc, "-cf", "-", ".")
	pipe, err := producer.StdoutPipe()
	if err != nil {
		return err
	}
	extractScript := fmt.Sprintf(`mkdir -p -- %s && tar -C %s -xf -`,
		shellQuote(forkDst), shellQuote(forkDst))
	consumer := forkNsCommand(pid, "/bin/sh", "-c", extractScript)
	consumer.Stdin = pipe
	return runPipeline(producer, consumer, "copy dir into fork")
}

// copyOut streams a file or directory out of the fork to the host. The fork
// path's type is unknown to the host, so it is probed inside the fork first;
// a file streams over cat (fast, exact), a directory over tar (recursive,
// attribute-preserving).
func copyOut(pid int, forkSrc, hostDst string) error {
	isDir, err := forkPathIsDir(pid, forkSrc)
	if err != nil {
		return err
	}
	if isDir {
		return copyDirOut(pid, forkSrc, hostDst)
	}
	return copyFileOut(pid, forkSrc, hostDst)
}

// forkPathIsDir reports whether forkSrc is a directory inside the fork. A
// missing path is a hard error (nothing to copy), distinguished from a
// regular file.
func forkPathIsDir(pid int, forkSrc string) (bool, error) {
	script := fmt.Sprintf(`if [ -d %s ]; then echo d; elif [ -e %s ]; then echo f; else echo n; fi`,
		shellQuote(forkSrc), shellQuote(forkSrc))
	cmd := forkNsCommand(pid, "/bin/sh", "-c", script)
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("probe fork path %s: %w", forkSrc, err)
	}
	switch strings.TrimSpace(string(out)) {
	case "d":
		return true, nil
	case "f":
		return false, nil
	default:
		return false, fmt.Errorf("fork path %s does not exist", forkSrc)
	}
}

// copyFileOut streams a fork file to the host. The host destination is created
// in the host mount namespace; the child, inside the fork, reads the fork path
// and its stdout is spliced to the host fd.
func copyFileOut(pid int, forkSrc, hostDst string) error {
	dst, err := os.OpenFile(hostDst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create host destination %s: %w", hostDst, err)
	}
	defer dst.Close()

	cmd := forkNsCommand(pid, "/bin/sh", "-c", fmt.Sprintf("cat -- %s", shellQuote(forkSrc)))
	cmd.Stdout = dst
	return runNsChild(cmd, "copy file out of fork")
}

// copyDirOut streams a fork directory tree to the host over tar. tar inside
// the fork produces the stream; the host side extracts it under the
// destination directory.
func copyDirOut(pid int, forkSrc, hostDst string) error {
	if err := os.MkdirAll(hostDst, 0o755); err != nil {
		return fmt.Errorf("create host destination %s: %w", hostDst, err)
	}
	producer := forkNsCommand(pid, "/bin/sh", "-c", fmt.Sprintf("tar -C %s -cf - .", shellQuote(forkSrc)))
	pipe, err := producer.StdoutPipe()
	if err != nil {
		return err
	}
	consumer := exec.Command("tar", "-C", hostDst, "-xf", "-")
	consumer.Stdin = pipe
	return runPipeline(producer, consumer, "copy dir out of fork")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// forkNsCommand builds a command that runs argv joined into every namespace of
// the target pid via nsenter (util-linux). nsenter does its setns
// single-threaded before exec, avoiding the Go-runtime mount-namespace setns
// hazard; it is a host runtime dependency of Waypoint regardless. The env is
// fixed to the guest's standard PATH — never the invoking user's.
func forkNsCommand(pid int, argv ...string) *exec.Cmd {
	args := append([]string{"-a", "-t", strconv.Itoa(pid), "--"}, argv...)
	cmd := exec.Command("nsenter", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	return cmd
}

func runNsChild(cmd *exec.Cmd, what string) error {
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if d := strings.TrimSpace(stderr.String()); d != "" {
			return fmt.Errorf("%s failed: %w: %s", what, err, d)
		}
		return fmt.Errorf("%s failed: %w", what, err)
	}
	return nil
}

// runPipeline runs producer | consumer, failing if either side errors.
func runPipeline(producer, consumer *exec.Cmd, what string) error {
	var pErr, cErr strings.Builder
	producer.Stderr = &pErr
	consumer.Stderr = &cErr
	if err := consumer.Start(); err != nil {
		return fmt.Errorf("%s: start consumer: %w", what, err)
	}
	if err := producer.Run(); err != nil {
		_ = consumer.Wait()
		return fmt.Errorf("%s: producer: %w: %s", what, err, strings.TrimSpace(pErr.String()))
	}
	if err := consumer.Wait(); err != nil {
		return fmt.Errorf("%s: consumer: %w: %s", what, err, strings.TrimSpace(cErr.String()))
	}
	return nil
}
