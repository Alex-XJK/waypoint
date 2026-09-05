package main

// bash_init supervises one long-lived interactive bash on a PTY inside the
// session root and exposes it as a request/response command service over a
// Unix socket. The whole tree (bash_init + bash + any user processes) is what
// CRIU checkpoints, so hidden process state (cwd, variables, background jobs,
// local servers) survives checkpoint/restore/fork.
//
// Exec protocol (v2, "WP2"):
//
//	request:  <decimal payload length>\n<payload bytes>
//	response: WP2 <ok|timeout|dead|output_limit|request_too_large> <exit-code>\n<raw output until close>
//
// "dead" means the shell process is gone (the command ran `exit`, or it
// crashed); the fork is no longer usable and bash_init exits shortly after.
//
// Command completion and exit codes travel out-of-band: every command is
// followed by a bash builtin that writes "<nonce> $?" to the FIFO at
// /.waypoint/exec.done. The PTY therefore carries only program output — echo
// and prompts are disabled — and no in-band marker parsing is needed.
// Clients that see a response without the WP2 header are talking to an older
// checkpointed bash_init and should treat the whole stream as v1 raw output.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// completionFifoGuestPath is the path bash redirects completion lines to,
// as seen from inside the session root.
const completionFifoGuestPath = "/.waypoint/exec.done"

// execTimeout bounds a single command. Clients normally control command
// lifetime by disconnecting (which terminates the foreground process group),
// so this is a backstop, not a policy.
const execTimeout = 24 * time.Hour

const (
	maxRequestHeaderBytes = 32
	maxCommandBytes       = 1 << 20  // 1 MiB
	maxCommandOutputBytes = 16 << 20 // 16 MiB
)

// resolveWorkDir picks the shell's starting directory from the optional third
// argument -- the built image's WORKDIR. An image that declares none, or one
// naming a path that is not a directory in the session rootfs, falls back to
// "/" with a warning rather than failing the session: a bad WORKDIR should not
// cost the user their environment.
func resolveWorkDir() string {
	if len(os.Args) < 4 || os.Args[3] == "" {
		return "/"
	}
	workDir := os.Args[3]
	// Called after the re-exec has pivoted into the session root, so the path
	// is checked as the session itself sees it -- no host prefix to join.
	if fi, err := os.Stat(workDir); err != nil || !fi.IsDir() {
		fmt.Fprintf(os.Stderr,
			"warning: image WORKDIR %q is not a usable directory, starting at /\n", workDir)
		return "/"
	}
	return workDir
}

func main() {
	if len(os.Args) < 3 {
		fmt.Println("Usage: bash_init <socket-path> <chroot-dir> [work-dir]")
		fmt.Println("Example: bash_init /tmp/bash_cmd.sock /tmp/waypoint-sessions/xyz/work /app")
		fmt.Println("  work-dir: starting directory inside the session, defaults to /")
		os.Exit(1)
	}

	socketPath := os.Args[1]
	chrootDir := os.Args[2]

	// Ensure chroot directory exists
	if _, err := os.Stat(chrootDir); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Chroot directory does not exist: %s\n", chrootDir)
		os.Exit(1)
	}
	// bash_init only runs inside fresh namespaces provided by waypoint
	// (StartShell's Cloneflags). Refuse anything else: the setup below
	// (pivot_root, sethostname) would otherwise mutate the host.
	if os.Getenv("WAYPOINT_NAMESPACED") != "1" {
		fmt.Fprintln(os.Stderr, "bash_init must be launched by waypoint inside fresh namespaces; refusing to run directly on the host")
		os.Exit(1)
	}
	if os.Getenv("WAYPOINT_REEXECED") != "1" {
		if err := setupNamespaceRuntime(chrootDir); err != nil {
			fmt.Fprintf(os.Stderr, "failed to set up namespace runtime: %v\n", err)
			os.Exit(1)
		}
		reexecPath := os.Getenv("WAYPOINT_REEXEC_PATH")
		if reexecPath == "" {
			reexecPath = "/.waypoint/bash_init"
		}
		env := append(os.Environ(), "WAYPOINT_REEXECED=1")
		// After the re-exec the process is pivoted into the session root, so
		// the chroot argument becomes "/" -- but the image work-dir must ride
		// along, or the shell would start at / regardless of the image.
		reexecArgs := []string{reexecPath, socketPath, "/"}
		if len(os.Args) >= 4 {
			reexecArgs = append(reexecArgs, os.Args[3])
		}
		if err := syscall.Exec(reexecPath, reexecArgs, env); err != nil {
			fmt.Fprintf(os.Stderr, "failed to re-exec %s: %v\n", reexecPath, err)
			os.Exit(1)
		}
	}
	chrootDir = "/"
	// Reap children (the bash we spawn, plus orphans reparented to us when
	// we are PID 1 of the namespace). We never call cmd.Wait, so this does
	// not race with os/exec.
	go reapOrphanedChildren()

	// Create PTY
	ptyMaster, ptySlave, err := pty.Open()
	if err != nil {
		panic(err)
	}
	// Non-canonical so long command lines are not truncated by the line
	// discipline, and no echo: with out-of-band completion the PTY carries
	// only program output. ISIG stays on so Ctrl-C can reset a shell stuck
	// in a continuation prompt.
	if err := setRawishNoEcho(ptySlave); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to set PTY mode: %v\n", err)
	}
	// Give the terminal a real size; otherwise programs see 0x0 and
	// width-aware output (pagers, tables, wrapping) misbehaves.
	if err := pty.Setsize(ptyMaster, &pty.Winsize{Rows: 40, Cols: 120}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to set PTY window size: %v\n", err)
	}

	completions, err := openCompletionFifo(filepath.Join(chrootDir, completionFifoGuestPath))
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to set up completion fifo: %v\n", err)
		os.Exit(1)
	}

	// Start bash with PTY
	cmd := exec.Command(
		"/bin/bash",
		"--noprofile",
		"--noediting",
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0,
	}
	cmd.Dir = resolveWorkDir()
	// The shell gets bash_init's environment (a fixed set chosen by
	// StartShell, not the invoking user's) minus the WAYPOINT_* plumbing
	// vars, which are bash_init implementation details.
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "WAYPOINT_") || strings.HasPrefix(kv, "TERM=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env, "TERM=xterm")
	cmd.Stdin = ptySlave
	cmd.Stdout = ptySlave
	cmd.Stderr = ptySlave

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start /bin/bash: %v\n", err)
		os.Exit(1)
	}

	bashPID := cmd.Process.Pid
	// Drop the pidfd os/exec opened for the child; a retained pidfd is an
	// anon inode CRIU cannot dump. We only ever address bash by numeric PID.
	if err := cmd.Process.Release(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to release bash process handle: %v\n", err)
	}

	// Drain PTY output continuously into a buffer.
	outputBuffer := newBoundedBuffer(maxCommandOutputBytes)
	go drainPTY(ptyMaster, outputBuffer)

	// Prove the shell executes commands and neutralize prompt output before
	// accepting clients: socket presence then means "shell is usable".
	if err := initShellSession(ptyMaster, completions); err != nil {
		fmt.Fprintf(os.Stderr, "shell failed startup handshake: %v\n", err)
		// A shell that died on startup usually said why on the PTY (e.g. the
		// loader naming a shared library the rootfs is missing).
		if out := strings.TrimSpace(outputBuffer.ReadAndClear()); out != "" {
			fmt.Fprintf(os.Stderr, "shell output:\n%s\n", out)
		}
		os.Exit(1)
	}
	outputBuffer.Reset() // discard rc-file/prompt noise from startup

	// Create Unix domain socket for command communication
	os.Remove(socketPath) // Clean up old socket

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		panic(err)
	}
	defer listener.Close()

	fmt.Println("Server pid:", os.Getpid())
	fmt.Println("Bash pid:", bashPID)
	fmt.Println("Socket path:", socketPath)
	fmt.Println("Ready to receive commands from Unix Domain Socket...")
	if os.Getenv("WAYPOINT_NAMESPACED") == "1" && os.Getenv("WAYPOINT_KEEP_STDIO") != "1" {
		if err := detachStandardFiles(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to detach stdio: %v\n", err)
		}
	}

	// The fork is useless once its shell is gone (`exit`, crash): exit so
	// the socket disappears and clients fail fast instead of hanging.
	go func() {
		for shellAlive(bashPID) {
			time.Sleep(500 * time.Millisecond)
		}
		time.Sleep(2 * time.Second) // let an in-flight "dead" response drain
		os.Exit(0)
	}()

	// Serializes command execution on the single shared shell.
	var shellMutex sync.Mutex

	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}

		go handleClient(conn, ptyMaster, &shellMutex, outputBuffer, bashPID, completions)
	}
}

// shellAlive reports whether the shell process still exists. Once the reaper
// collects it, the PID disappears from our namespace.
func shellAlive(bashPID int) bool {
	return unix.Kill(bashPID, 0) == nil
}

// openCompletionFifo creates (or reuses) the completion FIFO and returns a
// channel of lines written to it. Both ends are held open by bash_init: the
// read end feeds the channel, the dummy write end prevents EOF cycling.
// Because the FIFO is a named pipe in the session filesystem, CRIU can
// checkpoint these fds like any other.
func openCompletionFifo(hostPath string) (<-chan string, error) {
	if err := os.MkdirAll(filepath.Dir(hostPath), 0o755); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(hostPath); err == nil && fi.Mode()&os.ModeNamedPipe == 0 {
		if err := os.Remove(hostPath); err != nil {
			return nil, fmt.Errorf("cannot replace non-fifo %s: %w", hostPath, err)
		}
	}
	if err := unix.Mkfifo(hostPath, 0o666); err != nil && err != unix.EEXIST {
		return nil, fmt.Errorf("mkfifo %s: %w", hostPath, err)
	}

	readFD, err := unix.Open(hostPath, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open fifo for read: %w", err)
	}
	reader := os.NewFile(uintptr(readFD), hostPath)
	// Keep a writer open so reads block instead of returning EOF between
	// commands. Deliberately leaked for the process lifetime.
	if _, err := unix.Open(hostPath, unix.O_WRONLY|unix.O_NONBLOCK, 0); err != nil {
		reader.Close()
		return nil, fmt.Errorf("open fifo keeper writer: %w", err)
	}

	lines := make(chan string, 16)
	go func() {
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			default: // drop if nobody is waiting; nonces make stale lines harmless
			}
		}
	}()
	return lines, nil
}

// initShellSession blanks the prompt state (PS1/PS2/PROMPT_COMMAND may be set
// by rc files), turns off history expansion, and waits for the shell to
// acknowledge over the FIFO.
//
// History expansion is on by default in an interactive bash and off under
// `bash -c`, which is the environment callers write commands for. Left on,
// a `!` inside double quotes fails with "event not found" and `!!` is
// replaced by the previous input — which in a fork is always our own
// completion line — so `git commit -m "ship it!!"` would silently commit
// something else.
func initShellSession(ptyMaster *os.File, completions <-chan string) error {
	nonce := newNonce()
	init := fmt.Sprintf("unset PROMPT_COMMAND; PS1=; PS2=; set +H; builtin printf '%%s 0\\n' '%s' > %s\n",
		nonce, completionFifoGuestPath)
	if _, err := ptyMaster.WriteString(init); err != nil {
		return err
	}
	if _, ok := awaitCompletion(completions, nonce, 10*time.Second); !ok {
		return fmt.Errorf("no handshake from shell within 10s")
	}
	return nil
}

func newNonce() string {
	return fmt.Sprintf("wp%d", time.Now().UnixNano())
}

// parseCompletion extracts the exit code from a "<nonce> <exit-code>" line.
func parseCompletion(line, nonce string) (int, bool) {
	fields := strings.Fields(line)
	if len(fields) != 2 || fields[0] != nonce {
		return 0, false
	}
	code, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return code, true
}

// awaitCompletion waits for a "<nonce> <exit-code>" line, ignoring lines with
// other nonces (stale completions or stray writes to the FIFO).
func awaitCompletion(completions <-chan string, nonce string, timeout time.Duration) (int, bool) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case line := <-completions:
			if code, ok := parseCompletion(line, nonce); ok {
				return code, true
			}
		case <-deadline.C:
			return 0, false
		}
	}
}

func handleClient(conn net.Conn, ptyMaster *os.File, shellMutex *sync.Mutex, outputBuffer *boundedBuffer, bashPID int, completions <-chan string) {
	defer conn.Close()

	reader := bufio.NewReader(conn)

	// Read one length-prefixed command from client.
	payloadLen, err := readPayloadLength(reader)
	if err != nil {
		// The peer hung up before finishing its header. There is nobody left
		// to read a status, and "request_too_large" would misdescribe it in
		// the log; the remaining cases are genuine protocol violations.
		if errors.Is(err, io.EOF) {
			return
		}
		respond(conn, "request_too_large", 125, err.Error()+"\n")
		return
	}
	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return
	}
	command := string(payload)

	shellMutex.Lock()
	defer shellMutex.Unlock()

	if !shellAlive(bashPID) {
		respond(conn, "dead", 255, "")
		return
	}

	// Output produced between commands (background job spew) is not part of
	// any request/response exchange; drop it.
	outputBuffer.Reset()

	nonce := newNonce()
	if _, err := ptyMaster.WriteString(frameCommand(command, nonce, completionFifoGuestPath)); err != nil {
		return
	}

	// Watch for the client disconnecting mid-command so abandoned commands
	// do not run forever.
	clientClosed := make(chan struct{})
	serverDone := make(chan struct{})
	defer close(serverDone)
	go watchClient(conn, clientClosed, serverDone)

	// Single consumer of the completions channel: leaked concurrent readers
	// would steal later commands' completion lines.
	timeout := time.After(execTimeout)
	liveness := time.NewTicker(500 * time.Millisecond)
	defer liveness.Stop()
	for {
		select {
		case <-liveness.C:
			if outputBuffer.Exceeded() {
				interruptShell(ptyMaster, bashPID)
				awaitCompletion(completions, nonce, 2*time.Second)
				respond(conn, "output_limit", 125, collectOutput(outputBuffer))
				return
			}
			// The command killed the shell (e.g. `exit`): no completion is
			// coming. Report what output we have.
			if !shellAlive(bashPID) {
				respond(conn, "dead", 255, collectOutput(outputBuffer))
				return
			}

		case line := <-completions:
			code, ok := parseCompletion(line, nonce)
			if !ok {
				continue // stale nonce or stray write to the FIFO
			}
			output := collectOutput(outputBuffer)
			if outputBuffer.Exceeded() {
				respond(conn, "output_limit", 125, output)
				return
			}
			respond(conn, "ok", code, output)
			return

		case <-timeout:
			interruptShell(ptyMaster, bashPID)
			if code, ok := awaitCompletion(completions, nonce, 2*time.Second); ok {
				respond(conn, "ok", code, collectOutput(outputBuffer))
				return
			}
			respond(conn, "timeout", 124, collectOutput(outputBuffer))
			return

		case <-clientClosed:
			interruptShell(ptyMaster, bashPID)
			// Resync: give the completion line a moment to arrive so it does
			// not bleed into the next command's exchange.
			awaitCompletion(completions, nonce, 2*time.Second)
			return
		}
	}
}

// frameCommand turns one command payload into the bytes written to the PTY:
// the command, a blank line, then the completion line that reports `$?`
// through the FIFO at fifoPath. The blank line is not cosmetic. A payload
// ending in a backslash is a line continuation, and without the separator
// the completion line would be joined onto the command and run as its
// arguments — no completion would ever arrive. An empty line is a no-op to
// bash and leaves `$?` untouched, so the reported status is still the
// command's own.
func frameCommand(command, nonce, fifoPath string) string {
	if !strings.HasSuffix(command, "\n") {
		command += "\n"
	}
	return command + "\n" + fmt.Sprintf("builtin printf '%%s %%s\\n' '%s' \"$?\" > %s\n", nonce, fifoPath)
}

// interruptShell aborts whatever the shell is doing: Ctrl-C clears a parser
// stuck in a continuation (e.g. unterminated quote swallowed the completion
// line), and the foreground process group is terminated if one is running.
func interruptShell(ptyMaster *os.File, bashPID int) {
	_, _ = ptyMaster.Write([]byte{0x03})
	terminateForegroundIfAny(bashPID, 500*time.Millisecond)
}

// collectOutput reads the buffered PTY output for the command that just
// completed. The completion line arrives on a different fd than the PTY
// data, so wait for the PTY to go quiet briefly to catch trailing output.
func collectOutput(outputBuffer *boundedBuffer) string {
	var sb strings.Builder
	sb.WriteString(outputBuffer.ReadAndClear())
	deadline := time.Now().Add(250 * time.Millisecond)
	quietSince := time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		chunk := outputBuffer.ReadAndClear()
		if chunk != "" {
			sb.WriteString(chunk)
			quietSince = time.Now()
		} else if time.Since(quietSince) > 30*time.Millisecond {
			break
		}
	}
	// The PTY line discipline translates \n to \r\n on output; undo that.
	// Lone \r (progress bars etc.) is program behavior and passes through.
	return strings.ReplaceAll(sb.String(), "\r\n", "\n")
}

func respond(conn net.Conn, status string, code int, output string) {
	writer := bufio.NewWriter(conn)
	fmt.Fprintf(writer, "WP2 %s %d\n", status, code)
	writer.WriteString(output)
	writer.Flush()
}

// watchClient closes clientClosed when the peer disconnects.
func watchClient(conn net.Conn, clientClosed chan struct{}, serverDone <-chan struct{}) {
	buf := make([]byte, 1)
	for {
		select {
		case <-serverDone:
			return
		default:
		}
		_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		_, err := conn.Read(buf)
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			continue
		}
		if err == nil {
			continue // ignore unexpected extra client data
		}
		select {
		case <-serverDone:
		default:
			close(clientClosed)
		}
		return
	}
}

func detachStandardFiles() error {
	fd, err := unix.Open("/dev/null", unix.O_RDWR, 0)
	if err != nil {
		return err
	}
	if fd > 2 {
		defer unix.Close(fd)
	}

	for _, stdfd := range []int{0, 1, 2} {
		if err := unix.Dup2(fd, stdfd); err != nil {
			return err
		}
	}
	return nil
}

func setupNamespaceRuntime(chrootDir string) error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mount namespace private failed: %w", err)
	}

	// The session owns a fresh UTS namespace (CLONE_NEWUTS at launch), so
	// this names only the sandbox. Without the namespace, guests would
	// report — or as root, rename — the host's hostname.
	if err := unix.Sethostname([]byte("waypoint")); err != nil {
		return fmt.Errorf("set sandbox hostname failed: %w", err)
	}

	if err := pivotIntoSessionRoot(chrootDir); err != nil {
		return err
	}
	if err := mountDeviceRuntime(); err != nil {
		return err
	}

	newRoot := string(filepath.Separator)
	for _, mount := range []struct {
		rel    string
		source string
		fstype string
	}{
		{rel: "proc", source: "proc", fstype: "proc"},
		{rel: "sys", source: "sys", fstype: "sysfs"},
	} {
		target := filepath.Join(newRoot, mount.rel)
		if err := os.MkdirAll(target, 0o755); err != nil {
			return err
		}
		if err := unix.Mount(mount.source, target, mount.fstype, 0, ""); err != nil && err != syscall.EBUSY {
			return fmt.Errorf("mount %s on %s failed: %w", mount.fstype, target, err)
		}
	}
	return nil
}

func pivotIntoSessionRoot(newRoot string) error {
	newRoot = filepath.Clean(newRoot)
	if err := unix.Mount(newRoot, newRoot, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind mount new root %s failed: %w", newRoot, err)
	}

	oldRoot := filepath.Join(newRoot, ".waypoint-old-root")
	if err := os.MkdirAll(oldRoot, 0o755); err != nil {
		return fmt.Errorf("mkdir old root %s failed: %w", oldRoot, err)
	}
	if err := unix.PivotRoot(newRoot, oldRoot); err != nil {
		return fmt.Errorf("pivot_root into %s failed: %w", newRoot, err)
	}
	if err := unix.Chdir("/"); err != nil {
		return fmt.Errorf("chdir after pivot_root failed: %w", err)
	}
	if err := unix.Unmount("/.waypoint-old-root", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount old root failed: %w", err)
	}
	if err := os.RemoveAll("/.waypoint-old-root"); err != nil {
		return fmt.Errorf("remove old root marker failed: %w", err)
	}
	return nil
}

// mountDeviceRuntime assembles the session's /dev inside its private root.
// devtmpfs must never be mounted here: all devtmpfs mounts share one kernel
// superblock with the host's /dev, so file mutations — such as replacing
// /dev/ptmx below — would propagate to the host and break PTY allocation
// for every unprivileged host process (a private mount namespace isolates
// the mount table, not the contents of a shared filesystem). Instead the
// device nodes are created here, in the session rootfs itself, so every
// mutation stays in the session's copy-on-write root.
func mountDeviceRuntime() error {
	if err := os.MkdirAll("/dev", 0o755); err != nil {
		return err
	}

	// Standard character devices. This is the sole owner of sandbox device
	// nodes — images may ship none at all (e.g. Docker images with an empty
	// /dev). The chmod is not optional: mknod is subject to umask and images
	// ship varying modes.
	for _, d := range []struct {
		name         string
		major, minor uint32
	}{
		{"null", 1, 3},
		{"zero", 1, 5},
		{"full", 1, 7},
		{"random", 1, 8},
		{"urandom", 1, 9},
		{"tty", 5, 0},
	} {
		path := "/dev/" + d.name
		if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeCharDevice == 0 {
			// Some images ship these as symlinks or files; replace with a
			// real node (a chmod would follow or trip on the symlink).
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("remove non-device %s failed: %w", path, err)
			}
		}
		if _, err := os.Lstat(path); err != nil {
			if err := unix.Mknod(path, unix.S_IFCHR|0o666, int(unix.Mkdev(d.major, d.minor))); err != nil && err != unix.EEXIST {
				return fmt.Errorf("mknod %s failed: %w", path, err)
			}
		}
		if err := os.Chmod(path, 0o666); err != nil {
			return fmt.Errorf("chmod %s failed: %w", path, err)
		}
	}

	// Shell conveniences (process substitution, /dev/stdin redirections)
	// previously inherited from the host's devtmpfs.
	for _, l := range []struct{ link, target string }{
		{"/dev/fd", "/proc/self/fd"},
		{"/dev/stdin", "/proc/self/fd/0"},
		{"/dev/stdout", "/proc/self/fd/1"},
		{"/dev/stderr", "/proc/self/fd/2"},
	} {
		if err := os.Symlink(l.target, l.link); err != nil && !os.IsExist(err) {
			return fmt.Errorf("create %s symlink failed: %w", l.link, err)
		}
	}

	// POSIX shared memory on a session-private tmpfs. Older images ship
	// /dev/shm as a symlink (e.g. -> /run/shm); replace it so the mount
	// lands at the real path. The chmod backstops the umask-subject
	// MkdirAll for the EBUSY (already-mounted) path; the fresh mount's
	// mode=1777 covers the normal one.
	if fi, err := os.Lstat("/dev/shm"); err == nil && !fi.IsDir() {
		if err := os.Remove("/dev/shm"); err != nil {
			return fmt.Errorf("remove non-directory /dev/shm failed: %w", err)
		}
	}
	if err := os.MkdirAll("/dev/shm", 0o1777); err != nil {
		return err
	}
	if err := os.Chmod("/dev/shm", 0o1777); err != nil {
		return err
	}
	if err := unix.Mount("tmpfs", "/dev/shm", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=1777"); err != nil && err != syscall.EBUSY {
		return fmt.Errorf("mount tmpfs on /dev/shm failed: %w", err)
	}

	// Private PTY namespace, with its ptmx endpoint reachable at the
	// conventional /dev/ptmx path. The symlink is created in the session's
	// private root, so the host's /dev/ptmx is untouched.
	if err := os.MkdirAll("/dev/pts", 0o755); err != nil {
		return err
	}
	if err := unix.Mount("devpts", "/dev/pts", "devpts", unix.MS_NOSUID|unix.MS_NOEXEC, "newinstance,ptmxmode=0666,mode=0620"); err != nil && err != syscall.EBUSY {
		return fmt.Errorf("mount devpts on /dev/pts failed: %w", err)
	}
	if err := os.Remove("/dev/ptmx"); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale /dev/ptmx failed: %w", err)
	}
	if err := os.Symlink("pts/ptmx", "/dev/ptmx"); err != nil && !os.IsExist(err) {
		return fmt.Errorf("create /dev/ptmx symlink failed: %w", err)
	}
	return nil
}

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

func reapOrphanedChildren() {
	for {
		var status unix.WaitStatus
		_, err := unix.Wait4(-1, &status, 0, nil)
		if err == unix.ECHILD {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if err != nil {
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// setRawishNoEcho disables canonical mode (no line-length limits) and all
// echo (the PTY should carry program output only). Signals stay enabled so
// Ctrl-C works.
func setRawishNoEcho(tty *os.File) error {
	fd := int(tty.Fd())
	tio, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return err
	}
	tio.Lflag &^= unix.ICANON | unix.ECHO | unix.ECHOE | unix.ECHOK | unix.ECHONL | unix.ECHOCTL
	tio.Cc[unix.VMIN] = 1
	tio.Cc[unix.VTIME] = 0
	return unix.IoctlSetTermios(fd, unix.TCSETS, tio)
}

func readPayloadLength(reader *bufio.Reader) (int, error) {
	header := make([]byte, 0, maxRequestHeaderBytes)
	for len(header) < maxRequestHeaderBytes {
		b, err := reader.ReadByte()
		if err != nil {
			return 0, fmt.Errorf("read request header: %w", err)
		}
		if b == '\n' {
			length, err := strconv.Atoi(string(header))
			if err != nil || length < 0 {
				return 0, fmt.Errorf("invalid request length")
			}
			if length > maxCommandBytes {
				return 0, fmt.Errorf("request is %d bytes; limit is %d", length, maxCommandBytes)
			}
			return length, nil
		}
		if b < '0' || b > '9' {
			return 0, fmt.Errorf("invalid request length")
		}
		header = append(header, b)
	}
	return 0, fmt.Errorf("request header exceeds %d bytes", maxRequestHeaderBytes)
}

// boundedBuffer always consumes PTY bytes so the producer cannot block, but
// retains at most limit bytes and records truncation for the command handler.
type boundedBuffer struct {
	data     []byte
	limit    int
	captured int
	exceeded bool
	mu       sync.Mutex
}

func newBoundedBuffer(limit int) *boundedBuffer {
	return &boundedBuffer{data: make([]byte, 0, min(limit, 4096)), limit: limit}
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.limit - b.captured
	if remaining > 0 {
		n := min(remaining, len(p))
		b.data = append(b.data, p[:n]...)
		b.captured += n
	}
	if len(p) > remaining {
		b.exceeded = true
	}
	return len(p), nil
}

func (b *boundedBuffer) ReadAndClear() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	data := string(b.data)
	b.data = b.data[:0]
	return data
}

func (b *boundedBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = b.data[:0]
	b.captured = 0
	b.exceeded = false
}

func (b *boundedBuffer) Exceeded() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.exceeded
}

// drainPTY continuously reads from PTY and writes to buffer
func drainPTY(ptyMaster *os.File, outputBuffer *boundedBuffer) {
	buf := make([]byte, 4096)
	for {
		n, err := ptyMaster.Read(buf)
		if n > 0 {
			outputBuffer.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// readProcTPGID parses /proc/<pid>/stat and returns tpgid.
func readProcTPGID(bashPID int) (int, error) {
	path := fmt.Sprintf("/proc/%d/stat", bashPID)
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	// Find the closing parenthesis of comm
	content := string(data)
	rpar := strings.LastIndex(content, ")")
	if rpar == -1 {
		return 0, fmt.Errorf("malformed /proc stat: missing )")
	}
	rem := strings.TrimSpace(content[rpar+1:])
	fields := strings.Fields(rem)
	// need at least: state, ppid, pgrp, session, tty_nr, tpgid
	if len(fields) < 6 {
		return 0, fmt.Errorf("malformed /proc stat: insufficient fields")
	}
	return strconv.Atoi(fields[5])
}

// terminateForegroundIfAny sends SIGTERM to the current foreground process
// group of the PTY, escalating to SIGKILL after a grace period.
func terminateForegroundIfAny(bashPID int, grace time.Duration) {
	bashPGID, err := unix.Getpgid(bashPID)
	if err != nil {
		return
	}

	fgPGID, err := readProcTPGID(bashPID)
	if err != nil {
		return
	}
	if fgPGID == bashPGID || fgPGID <= 0 {
		return // foreground is bash itself; nothing to terminate
	}
	target := fgPGID

	_ = unix.Kill(-target, unix.SIGTERM)

	if grace > 0 {
		time.Sleep(grace)
		currentFG, err := readProcTPGID(bashPID)
		if err != nil || currentFG != target {
			return
		}
		if err := unix.Kill(-target, 0); err == nil {
			_ = unix.Kill(-target, unix.SIGKILL)
		}
	}
}
