package main

// pty-rpc-shell: A RPC shell implementation that turns an interactive process into a request–response execution service.
//
// GitHub Repository: https://github.com/Alex-XJK/pty-rpc-shell.git
// Designed and developed by Alex Jiakai Xu (https://alex-xjk.github.io/), DAPLab @ Columbia University (https://daplab.cs.columbia.edu/)

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// Compile regexes once at package level for performance
var (
	ansiEscapeRegex  = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)
	oscSequenceRegex = regexp.MustCompile(`\x1b\][^\x07]*\x07`)
	otherEscapeRegex = regexp.MustCompile(`\x1b[>=]`)
)

func main() {
	if len(os.Args) < 3 {
		fmt.Println("Usage: bash_init <socket-path> <chroot-dir>")
		fmt.Println("Example: bash_init /tmp/bash_cmd.sock /tmp/waypoint-sessions/xyz/work")
		os.Exit(1)
	}

	socketPath := os.Args[1]
	chrootDir := os.Args[2]

	// Ensure chroot directory exists
	if _, err := os.Stat(chrootDir); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Chroot directory does not exist: %s\n", chrootDir)
		os.Exit(1)
	}
	if os.Getenv("WAYPOINT_NAMESPACED") == "1" {
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
			if err := syscall.Exec(reexecPath, []string{reexecPath, socketPath, "/"}, env); err != nil {
				fmt.Fprintf(os.Stderr, "failed to re-exec %s: %v\n", reexecPath, err)
				os.Exit(1)
			}
		}
		chrootDir = "/"
		go reapOrphanedChildren()
	}

	// Create PTY
	ptyMaster, ptySlave, err := pty.Open()
	if err != nil {
		panic(err)
	}
	// Disable canonical mode on the slave PTY so long single-line commands
	// are not truncated by line discipline limits (commonly ~4096 bytes).
	// Keep ECHO enabled because output cleaning logic depends on echoed input.
	// Also disable ECHOCTL so control chars are not shown as caret notation
	// (e.g., "^J"), which pollutes captured command output.
	if err := setNonCanonicalWithEcho(ptySlave); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to set non-canonical PTY mode: %v\n", err)
	}

	// Create Unix domain socket for command communication
	os.Remove(socketPath) // Clean up old socket

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		panic(err)
	}
	defer listener.Close()

	// Start bash with PTY
	cmd := exec.Command(
		"/bin/bash",
		"--noprofile",
		"--noediting",
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Chroot:  chrootDir,
		Setsid:  true,
		Setctty: true,
		Ctty:    0,
	}
	cmd.Dir = "/"
	cmd.Stdin = ptySlave
	cmd.Stdout = ptySlave
	cmd.Stderr = ptySlave

	if err := cmd.Start(); err != nil {
		panic(err)
	}

	bashPID := cmd.Process.Pid

	fmt.Println("Server pid:", os.Getpid())
	fmt.Println("Bash pid:", bashPID)
	fmt.Println("Socket path:", socketPath)
	fmt.Println("Ready to receive commands from Unix Domain Socket...")

	// Mutex to protect PTY reads/writes
	var ptyMutex sync.Mutex

	// Start a goroutine to continuously drain PTY output into a buffer
	outputBuffer := &syncBuffer{buf: &bytes.Buffer{}}
	go drainPTY(ptyMaster, outputBuffer)

	// Handle command connections
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}

		go handleClient(conn, ptyMaster, ptySlave, &ptyMutex, outputBuffer, bashPID)
	}
}

func setupNamespaceRuntime(chrootDir string) error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mount namespace private failed: %w", err)
	}

	if err := pivotIntoSessionRoot(chrootDir); err != nil {
		return err
	}
	if err := mountDeviceRuntime(); err != nil {
		return err
	}

	for _, mount := range []struct {
		rel    string
		source string
		fstype string
	}{
		{rel: "proc", source: "proc", fstype: "proc"},
		{rel: "sys", source: "sys", fstype: "sysfs"},
	} {
		target := filepath.Join(chrootDir, mount.rel)
		if err := os.MkdirAll(target, 0o755); err != nil {
			return err
		}
		if err := unix.Mount(mount.source, target, mount.fstype, 0, ""); err != nil && err != syscall.EBUSY {
			return fmt.Errorf("mount %s on %s failed: %w", mount.fstype, target, err)
		}
	}
	return bringLoopbackUp()
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

func mountDeviceRuntime() error {
	if err := os.MkdirAll("/dev", 0o755); err != nil {
		return err
	}
	if err := unix.Mount("devtmpfs", "/dev", "devtmpfs", unix.MS_NOSUID, "mode=755"); err != nil && err != syscall.EBUSY {
		return fmt.Errorf("mount devtmpfs on /dev failed: %w", err)
	}
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

func setNonCanonicalWithEcho(tty *os.File) error {
	fd := int(tty.Fd())
	tio, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return err
	}
	// Disable canonical mode only; keep echoing enabled.
	tio.Lflag &^= unix.ICANON
	// Do not render control chars via caret notation (e.g., "^J").
	tio.Lflag &^= unix.ECHOCTL
	// Ensure read returns as soon as at least one byte is available.
	tio.Cc[unix.VMIN] = 1
	tio.Cc[unix.VTIME] = 0
	return unix.IoctlSetTermios(fd, unix.TCSETS, tio)
}

// syncBuffer is a thread-safe buffer
type syncBuffer struct {
	buf *bytes.Buffer
	mu  sync.Mutex
}

func (sb *syncBuffer) Write(p []byte) (n int, err error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.buf.Write(p)
}

func (sb *syncBuffer) ReadAndClear() string {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	data := sb.buf.String()
	sb.buf.Reset()
	return data
}

// drainPTY continuously reads from PTY and writes to buffer
func drainPTY(ptyMaster *os.File, outputBuffer *syncBuffer) {
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

// buildWrappedMarkerRegex matches a marker even if PTY line wrapping inserts '\n' inside it.
func buildWrappedMarkerRegex(marker string) *regexp.Regexp {
	parts := make([]string, 0, len(marker))
	for _, r := range marker {
		parts = append(parts, regexp.QuoteMeta(string(r)))
	}
	return regexp.MustCompile(strings.Join(parts, `\n*`))
}

func handleClient(conn net.Conn, ptyMaster *os.File, ptySlave *os.File, ptyMutex *sync.Mutex, outputBuffer *syncBuffer, bashPID int) {
	defer conn.Close()

	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)

	// Read one length-prefixed command from client.
	// Protocol:
	//   <decimal byte length>\n
	//   <raw command bytes>
	lenLine, err := reader.ReadString('\n')
	if err != nil {
		fmt.Fprintf(os.Stderr, "read error: %v\n", err)
		return
	}
	lenText := strings.TrimSpace(lenLine)
	payloadLen, err := strconv.Atoi(lenText)
	if err != nil || payloadLen < 0 {
		fmt.Fprintf(os.Stderr, "invalid command length %q: %v\n", lenText, err)
		return
	}
	fmt.Printf("Protocol >> Length [%d]\n", payloadLen)

	payload := make([]byte, payloadLen)
	_, err = io.ReadFull(reader, payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to read command payload: %v\n", err)
		return
	}
	commandPayload := string(payload)

	ptyMutex.Lock()
	defer ptyMutex.Unlock()

	// Clear any stale output before sending command
	staleOutput := outputBuffer.ReadAndClear()
	if staleOutput != "" {
		fmt.Println("Cleanup >> ===== Start =====")
		fmt.Printf("%s\n", staleOutput)
		fmt.Println("Cleanup >> ===== End =====")
	}

	// Generate a unique marker for this command.
	markerNonce := time.Now().UnixNano()
	marker := fmt.Sprintf("__CMD_DONE_%d_%d__", bashPID, markerNonce)
	markerRegex := buildWrappedMarkerRegex(marker)

	fmt.Println("Recv >> ===== Start =====")
	fmt.Print(commandPayload)
	fmt.Println("Recv >> ===== End =====")

	// Write command to PTY with a marker command appended on a new line.
	// This preserves multi-line shell constructs (e.g., heredoc) in the payload.
	cmdWithMarker := commandPayload
	if !strings.HasSuffix(cmdWithMarker, "\n") {
		cmdWithMarker += "\n"
	}
	cmdWithMarker += fmt.Sprintf("builtin printf '\\n%%s\\n' \"__CMD_DONE_$$_%d__\"\n", markerNonce)
	_, err = ptyMaster.WriteString(cmdWithMarker)
	if err != nil {
		fmt.Fprintf(os.Stderr, "write error: %v\n", err)
		return
	}

	// Wait for output with timeout
	timeout := time.After(60000 * time.Second)
	checkInterval := 5 * time.Millisecond // PTY scan cadence; low overhead
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	// Peer liveness watcher
	clientClosed := make(chan struct{})
	serverDone := make(chan struct{})
	go func() {
		fmt.Println("Watcher >> Start client liveness watcher")
		// After we read the command line, server does not read further data from the client.
		buf := make([]byte, 1)
		for {
			select {
			case <-serverDone:
				return
			default:
			}
			// Short read deadline to poll liveness.
			_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			_, err := conn.Read(buf)
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			// Any non-timeout error => client is closed or unreachable.
			select {
			case <-serverDone:
				return
			default:
			}
			fmt.Println("Watcher >> Client disconnected")
			close(clientClosed)
			return
		}
	}()

	var allOutput strings.Builder

	for {
		select {
		case <-timeout:
			// Timeout: terminate foreground process group (if any), then send what we have
			fmt.Println("Killer >> Timeout reached; attempt to terminate foreground process group")
			terminateForegroundIfAny(ptySlave, bashPID, 500*time.Millisecond)
			finalOutput := cleanOutput(allOutput.String(), cmdWithMarker, markerRegex)
			fmt.Printf("Watcher >> Writing collected output after timeout: %d bytes\n", len(finalOutput))
			close(serverDone)
			writer.WriteString(finalOutput)
			writer.Flush()
			return

		case <-clientClosed:
			// Client disconnected: terminate foreground process group (if any).
			fmt.Println("Killer >> Client disconnected; attempt to terminate foreground process group")
			terminateForegroundIfAny(ptySlave, bashPID, 500*time.Millisecond)
			close(serverDone)
			fmt.Println("Watcher >> Raw buffered outputs before terminated ")
			fmt.Println("Watcher >> ===== Start =====")
			fmt.Printf("%s\n", allOutput.String())
			fmt.Println("Watcher >> ===== End =====")
			return

		case <-ticker.C:
			// Check if we have output with the marker
			output := outputBuffer.ReadAndClear()
			if output != "" {
				allOutput.WriteString(output)

				normalized := stripControlChars(allOutput.String())
				if markerRegex.MatchString(normalized) {
					// Found actual marker - clean up output and send
					finalOutput := cleanOutput(allOutput.String(), cmdWithMarker, markerRegex)
					close(serverDone)
					writer.WriteString(finalOutput)
					writer.Flush()
					return
				}
			}
		}
	}
}

// stripControlChars removes ANSI escape sequences - optimized version
func stripControlChars(s string) string {
	// Remove ANSI escape sequences
	s = ansiEscapeRegex.ReplaceAllString(s, "")
	s = oscSequenceRegex.ReplaceAllString(s, "")
	s = otherEscapeRegex.ReplaceAllString(s, "")

	// Normalize line endings
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")

	return s
}

// cleanOutput removes command echo and marker, leaving only actual output
func cleanOutput(raw, cmdSent string, markerRegex *regexp.Regexp) string {
	fmt.Println("Raw >> ===== Start =====")
	fmt.Printf("%q\n", raw)
	fmt.Println("Raw >> ===== End =====")

	// Strip control characters
	cleaned := stripControlChars(raw)

	// Remove the actual marker, even if PTY wrapping split it across lines.
	cleaned = markerRegex.ReplaceAllString(cleaned, "")

	// Use a byte-budget approach to skip the echoed command across wrapped lines.
	// Initialize with the exact length of the sent command (including trailing newline).
	remainingEcho := len(cmdSent)

	lines := strings.Split(cleaned, "\n")
	var result []string

	for i, line := range lines {
		fmt.Printf("Line %d: %q\n", i, line)

		trimmed := strings.TrimSpace(line)

		// While we are still within the echoed command byte budget, drop lines entirely.
		// Account for the implicit '\n' that was removed by strings.Split by adding 1.
		if remainingEcho > 0 {
			fmt.Println("Judge >> Echoed command (budget)")
			remainingEcho -= len(line) + 1
			continue
		}

		// Skip empty lines
		if trimmed == "" {
			fmt.Println("Judge >> Empty line")
			continue
		}

		// Skip bash prompt patterns
		if ((strings.HasPrefix(trimmed, "bash-") || strings.Contains(trimmed, "@")) && (strings.HasSuffix(trimmed, "#") || strings.HasSuffix(trimmed, "$"))) ||
			trimmed == "$" || trimmed == "#" {
			fmt.Println("Judge >> Bash prompt line")
			continue
		}

		result = append(result, line)
		fmt.Println("Judge >> Keep!")
	}

	stringResults := strings.Join(result, "\n")
	stringResults = strings.Trim(stringResults, "\n")

	fmt.Println("Return >> ===== Start =====")
	fmt.Printf("%s\n", stringResults)
	fmt.Println("Return >> ===== End =====")

	return stringResults
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
	tpgidStr := fields[5]
	tpgid, err := strconv.Atoi(tpgidStr)
	if err != nil {
		return 0, err
	}
	return tpgid, nil
}

// terminateForegroundIfAny sends SIGTERM to the current foreground process group of the PTY
func terminateForegroundIfAny(tty *os.File, bashPID int, grace time.Duration) {
	// Resolve bash's own process group id.
	bashPGID, err := unix.Getpgid(bashPID)
	if err != nil {
		fmt.Printf("Killer >> Getpgid(bashPID=%d) error: %v\n", bashPID, err)
		return
	}
	fmt.Printf("Killer >> bashPGID=%d\n", bashPGID)

	// Snapshot the current foreground process group as the termination target.
	fgPGID, err := readProcTPGID(bashPID)
	if err != nil {
		fmt.Printf("Killer >> readProcTPGID error: %v\n", err)
		return
	}
	fmt.Printf("Killer >> foregroundPGID=%d\n", fgPGID)
	if fgPGID == bashPGID || fgPGID <= 0 {
		fmt.Println("Killer >> Foreground is bash or invalid; skip terminate")
		return
	}
	target := fgPGID

	// Send SIGTERM to the whole foreground process group.
	if err := unix.Kill(-target, unix.SIGTERM); err != nil {
		fmt.Printf("Killer >> SIGTERM to pgrp %d failed: %v\n", target, err)
	} else {
		fmt.Printf("Killer >> SIGTERM sent to pgrp %d\n", target)
	}

	// Optional graceful window before a hard kill.
	if grace > 0 {
		time.Sleep(grace)

		// Re-check: only hard kill if the same group is still in foreground and still alive.
		currentFG, errFG := readProcTPGID(bashPID)
		if errFG != nil {
			fmt.Printf("Killer >> readProcTPGID (post-term) error: %v\n", errFG)
			return
		}
		fmt.Printf("Killer >> foregroundPGID (post-term)=%d\n", currentFG)
		if currentFG == target {
			if err := unix.Kill(-target, 0); err == nil {
				fmt.Printf("Killer >> pgrp %d still alive; sending SIGKILL\n", target)
				if errK := unix.Kill(-target, unix.SIGKILL); errK != nil {
					fmt.Printf("Killer >> SIGKILL to pgrp %d failed: %v\n", target, errK)
				}
			} else {
				fmt.Printf("Killer >> pgrp %d not alive or kill -0 error: %v\n", target, err)
			}
		} else {
			fmt.Printf("Killer >> Foreground changed (%d -> %d); skip SIGKILL\n", target, currentFG)
		}
	}
}
