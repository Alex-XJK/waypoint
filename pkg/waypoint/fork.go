package waypoint

// Live forks: the Fork record and its persistence, fork teardown, and the
// client half of the exec protocol used to run commands in a fork's
// persistent shell (the server half lives in cmd/bash-init).

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const ForkStateFile = "fork.json"
const MainForkID = "main"

type ForkStatus string

const (
	ForkStatusStarting  ForkStatus = "starting"
	ForkStatusRunning   ForkStatus = "running"
	ForkStatusSnapshot  ForkStatus = "snapshotting"
	ForkStatusFailed    ForkStatus = "failed"
	ForkStatusDestroyed ForkStatus = "destroyed"
)

type Fork struct {
	ID               string   `json:"id"`
	SessionID        string   `json:"session_id"`
	BaseCheckpointID string   `json:"base_checkpoint_id"`
	LayerIDs         []string `json:"layer_ids"`
	OriginalDir      string   `json:"original_dir"`
	RootDir          string   `json:"root_dir"`
	UpperDir         string   `json:"upper_dir"`
	WorkDir          string   `json:"work_dir"`
	TempDir          string   `json:"temp_dir"`
	CanonicalTempDir string   `json:"canonical_temp_dir"`
	MountPoint       string   `json:"mount_point"`
	SocketPath       string   `json:"socket_path"`
	CanonicalSocket  string   `json:"canonical_socket"`
	LogPath          string   `json:"log_path"`
	CriuPath         string   `json:"criu_path"`
	PidFile          string   `json:"pid_file"`
	PID              int      `json:"pid"`
	// StartTime is the PID's /proc stat starttime, recorded at spawn so
	// kills can verify the identity (PID-reuse safety). 0 = unknown.
	StartTime       uint64     `json:"start_time,omitempty"`
	CreatedAt       int64      `json:"created_at"`
	RestoreDuration string     `json:"restore_duration,omitempty"`
	Status          ForkStatus `json:"status"`
	// RestoreBreakdown is the phase timing of this fork's most recent
	// restore (instrumentation; see criustats.go).
	RestoreBreakdown *RestoreBreakdown `json:"restore_breakdown,omitempty"`
}

// --- fork record persistence ---

func (m *Manager) saveFork(f *Fork) error {
	if err := os.MkdirAll(f.RootDir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(f.RootDir, ForkStateFile), data, 0o644)
}

func (m *Manager) loadFork(forkID string) (*Fork, error) {
	return loadForkFile(filepath.Join(m.forkDir(forkID), ForkStateFile))
}

func loadForkFile(path string) (*Fork, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f Fork
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

func (m *Manager) ListForks() ([]*Fork, error) {
	entries, err := os.ReadDir(m.forksDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	forks := make([]*Fork, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		f, err := m.loadFork(entry.Name())
		if err != nil {
			continue
		}
		forks = append(forks, f)
	}
	return forks, nil
}

// newForkRecord builds the record for a fresh fork of a checkpoint. The fork
// gets private upper/work/temp dirs; the canonical socket path is shared by
// all forks because it is baked into the CRIU image.
func newForkRecord(m *Manager, checkpointID string, metadata *Metadata, spec ForkSpec) (*Fork, error) {
	forkID := spec.ID
	if forkID == "" {
		id, err := generateSessionID()
		if err != nil {
			return nil, err
		}
		forkID = "fork-" + id
	}

	rootDir := m.forkDir(forkID)
	tempDir := filepath.Join(rootDir, "temp")
	originalDir := metadata.OriginalDir
	if originalDir == "" {
		originalDir = m.originalDir
	}
	canonicalSocket := m.canonicalSocketPath()

	return &Fork{
		ID:               forkID,
		SessionID:        m.sessionID,
		BaseCheckpointID: checkpointID,
		LayerIDs:         append([]string(nil), metadata.LayerIDs...),
		OriginalDir:      originalDir,
		RootDir:          rootDir,
		UpperDir:         filepath.Join(rootDir, "upper"),
		WorkDir:          filepath.Join(rootDir, "work"),
		TempDir:          tempDir,
		CanonicalTempDir: filepath.Dir(canonicalSocket),
		MountPoint:       m.workOverlay,
		SocketPath:       filepath.Join(tempDir, filepath.Base(canonicalSocket)),
		CanonicalSocket:  canonicalSocket,
		LogPath:          filepath.Join(rootDir, "restore.log"),
		CriuPath:         m.checkpointCriuDir(checkpointID),
		PidFile:          filepath.Join(rootDir, "restore.pid"),
		CreatedAt:        time.Now().Unix(),
		Status:           ForkStatusStarting,
	}, nil
}

// saveMainFork records the `main` fork right after `init --shell`; main is
// just another fork, running directly on the session's work overlay.
func (m *Manager) saveMainFork(pid int, socketPath, canonicalSocket, logPath string) error {
	rootDir := m.forkDir(MainForkID)
	f := &Fork{
		ID:              MainForkID,
		SessionID:       m.sessionID,
		OriginalDir:     m.originalDir,
		RootDir:         rootDir,
		UpperDir:        filepath.Join(rootDir, "upper"),
		WorkDir:         filepath.Join(rootDir, "work"),
		TempDir:         filepath.Join(rootDir, "temp"),
		MountPoint:      m.workOverlay,
		SocketPath:      socketPath,
		CanonicalSocket: canonicalSocket,
		LogPath:         logPath,
		PID:             pid,
		StartTime:       m.shellStartTime,
		CreatedAt:       time.Now().Unix(),
		Status:          ForkStatusRunning,
	}
	return m.saveFork(f)
}

// --- fork lifecycle ---

func (m *Manager) DestroyFork(forkID string) error {
	return m.withForkLock(forkID, func() error {
		f, err := m.loadFork(forkID)
		if err != nil {
			return err
		}
		f.Status = ForkStatusDestroyed
		if err := m.saveFork(f); err != nil {
			return err
		}
		if f.PID > 0 {
			if err := killTree(f.PID, f.StartTime); err != nil {
				return err
			}
		}
		_ = os.Remove(f.SocketPath)
		return os.RemoveAll(f.RootDir)
	})
}

// ExecuteForkCommand runs one shell command string in the fork's persistent
// shell. Extra args are joined with spaces into the command string, so the
// payload is always a single bash input, not an argv.
func (m *Manager) ExecuteForkCommand(forkID, command string, args ...string) (*ExecResult, error) {
	var result *ExecResult
	err := m.withForkLock(forkID, func() error {
		f, err := m.loadFork(forkID)
		if err != nil {
			return err
		}
		if f.Status != ForkStatusRunning {
			return fmt.Errorf("fork %s is not running (status=%s)", forkID, f.Status)
		}
		commandString := command
		if len(args) > 0 {
			commandString += " " + strings.Join(args, " ")
		}
		commandString += "\n"
		var execErr error
		result, execErr = execCommand(f.SocketPath, commandString)
		if errors.Is(execErr, ErrForkShellDead) {
			f.Status = ForkStatusFailed
			_ = m.saveFork(f)
		}
		return execErr
	})
	return result, err
}

// --- exec protocol client ---

// ErrForkShellDead reports that the fork's shell process is gone (the
// command ran `exit`, or bash crashed); the fork is no longer usable.
var ErrForkShellDead = errors.New("fork shell has exited")

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
			return nil, fmt.Errorf("%w; the fork is no longer usable (output: %q)", ErrForkShellDead, string(rest))
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

// --- small shared helpers ---

func readPIDFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("invalid pidfile %s: %w", path, err)
	}
	return pid, nil
}

// socketPathThroughProcRoot rewrites a fork's canonical socket path so the
// host can dial it through the fork's mount namespace.
func socketPathThroughProcRoot(pid int, canonicalSocket string) string {
	return filepath.Join("/proc", strconv.Itoa(pid), "root", strings.TrimPrefix(canonicalSocket, string(filepath.Separator)))
}

func dialUnixSocket(path string, timeout time.Duration) error {
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return err
	}
	return conn.Close()
}

func waitForForkSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if lastErr = dialUnixSocket(path, 100*time.Millisecond); lastErr == nil {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("socket %s did not become dialable: %v", path, lastErr)
}
