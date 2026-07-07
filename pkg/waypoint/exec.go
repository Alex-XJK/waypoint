package waypoint

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// ExecResult is the outcome of one command executed in a fork's shell.
type ExecResult struct {
	Output   string
	ExitCode int
	TimedOut bool
}

// clientReadTimeout bounds how long the client waits for a response. Command
// lifetime is otherwise controlled by the server: if this client goes away,
// the server terminates the command's foreground process group.
const clientReadTimeout = 24 * time.Hour

// execCommand sends one command to a fork's bash_init socket and parses the
// response. Protocol v2 responses carry a "WP2 <status> <exit-code>" header
// line; anything else is treated as v1 raw output (a bash_init checkpointed
// before the protocol change), with the exit code unknown.
func execCommand(socketPath, command string) (*ExecResult, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to shell socket: %w", err)
	}
	defer conn.Close()

	writer := bufio.NewWriter(conn)
	if _, err := fmt.Fprintf(writer, "%d\n%s", len(command), command); err != nil {
		return nil, fmt.Errorf("failed to send command: %w", err)
	}
	if err := writer.Flush(); err != nil {
		return nil, fmt.Errorf("failed to send command: %w", err)
	}

	conn.SetReadDeadline(time.Now().Add(clientReadTimeout))
	reader := bufio.NewReader(conn)
	header, headerErr := reader.ReadString('\n')

	if status, code, ok := parseResponseHeader(header); ok {
		rest, err := io.ReadAll(reader)
		if err != nil {
			return nil, fmt.Errorf("failed to read command output: %w", err)
		}
		if status == "dead" {
			return nil, fmt.Errorf("fork shell has exited; the fork is no longer usable (output: %q)", string(rest))
		}
		return &ExecResult{
			Output:   string(rest),
			ExitCode: code,
			TimedOut: status == "timeout",
		}, nil
	}

	// v1 fallback: the whole stream is output.
	if headerErr != nil && headerErr != io.EOF {
		return nil, fmt.Errorf("failed to read command output: %w", headerErr)
	}
	rest, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read command output: %w", err)
	}
	return &ExecResult{Output: header + string(rest)}, nil
}

func parseResponseHeader(line string) (status string, code int, ok bool) {
	fields := strings.Fields(line)
	if len(fields) != 3 || fields[0] != "WP2" {
		return "", 0, false
	}
	code, err := strconv.Atoi(fields[2])
	if err != nil {
		return "", 0, false
	}
	return fields[1], code, true
}
