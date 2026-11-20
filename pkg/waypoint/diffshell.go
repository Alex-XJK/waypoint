package waypoint

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/checkpoint-restore/go-criu/v8/crit"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/fdinfo"
	criu_fs "github.com/checkpoint-restore/go-criu/v8/crit/images/fs"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/tty"
	"golang.org/x/sys/unix"
)

// ShellDiff is the `shell` part of a diff result: what changed in the session
// shell, the bash that bash_init runs on a pty. It is read from the CRIU
// images, so it holds what the kernel knows about the shell (working
// directory, umask, terminal) and not what lives in bash's memory (variables,
// functions, aliases).
type ShellDiff struct {
	Records []ShellChange `json:"records"`
}

// Kinds of ShellChange.
const (
	ShellKindShell      = "shell"      // the shell itself: its pid, or null when there is none
	ShellKindCwd        = "cwd"        // working directory
	ShellKindUmask      = "umask"      // four octal digits
	ShellKindForeground = "foreground" // processes of the terminal's foreground job, empty when the shell is idle
	ShellKindTTY        = "tty"        // a terminal property, told apart by Name
)

// Names of ShellKindTTY changes.
const (
	TTYNameSize      = "size"      // TTYSize
	TTYNameEcho      = "echo"      // input is echoed
	TTYNameCanonical = "canonical" // input is line-buffered
	TTYNameSignals   = "signals"   // Ctrl-C and friends raise signals
	TTYNameTermios   = "termios"   // any other terminal setting, as TTYTermios
)

type ShellChange struct {
	Kind string `json:"kind"`
	Name string `json:"name,omitempty"`
	From any    `json:"from"`
	To   any    `json:"to"`
}

// ShellProc is one process of the terminal's foreground job.
type ShellProc struct {
	PID  uint32 `json:"pid"`
	Comm string `json:"comm"`
}

type TTYSize struct {
	Rows uint32 `json:"rows"`
	Cols uint32 `json:"cols"`
}

// TTYTermios is the raw terminal settings, flag words and control characters in hex.
type TTYTermios struct {
	Iflag string `json:"iflag"`
	Oflag string `json:"oflag"`
	Cflag string `json:"cflag"`
	Lflag string `json:"lflag"`
	Line  uint32 `json:"line"`
	Cc    string `json:"cc"`
}

// shellState is what one checkpoint's CRIU images say about the session shell.
type shellState struct {
	pid        uint32
	cwd        string
	umask      string // four octal digits; empty when the image has none
	foreground []ShellProc
	size       TTYSize
	termios    *tty.TermiosEntry
}

// ptyInfo merges the tty-info entries of one pty: the image has one entry for
// the master and one for the slave, and only one of them carries the session.
type ptyInfo struct {
	index   uint32
	sid     uint32
	pgrp    uint32
	termios *tty.TermiosEntry
	winsize *tty.WinsizeEntry
}

// critEntries decodes the CRIU image at path; decode names its entry type.
func critEntries(path string, decode func(crit.Critter) (*crit.CriuImage, error)) ([]*crit.CriuEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, err := decode(crit.New(f, nil, "", false, false))
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return img.Entries, nil
}

// readPtys returns the ptys in criuDir's tty-info.img by pty index. A
// checkpoint with no terminal has no such image and gives an empty map.
func readPtys(criuDir string) (map[uint32]*ptyInfo, error) {
	entries, err := critEntries(filepath.Join(criuDir, "tty-info.img"), func(c crit.Critter) (*crit.CriuImage, error) {
		return c.Decode(&tty.TtyInfoEntry{})
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ptys := make(map[uint32]*ptyInfo)
	for _, entry := range entries {
		info, ok := entry.Message.(*tty.TtyInfoEntry)
		if !ok {
			return nil, fmt.Errorf("unexpected tty-info entry type")
		}
		if info.GetType() != tty.TtyType_PTY || info.GetPty() == nil {
			continue
		}
		index := info.GetPty().GetIndex()
		p := ptys[index]
		if p == nil {
			p = &ptyInfo{index: index}
			ptys[index] = p
		}
		if info.GetSid() != 0 {
			p.sid = info.GetSid()
			p.pgrp = info.GetPgrp()
		}
		if p.termios == nil {
			p.termios = info.GetTermios()
		}
		if p.winsize == nil {
			p.winsize = info.GetWinsize()
		}
	}
	return ptys, nil
}

// readCwdAndUmask returns the working directory and umask of pid from
// fs-<pid>.img, with the directory's path looked up in files.img.
func readCwdAndUmask(criuDir string, pid uint32) (string, string, error) {
	entries, err := critEntries(filepath.Join(criuDir, fmt.Sprintf("fs-%d.img", pid)), func(c crit.Critter) (*crit.CriuImage, error) {
		return c.Decode(&criu_fs.FsEntry{})
	})
	if err != nil {
		return "", "", err
	}
	if len(entries) == 0 {
		return "", "", fmt.Errorf("no fs entry for PID %d", pid)
	}
	fsEntry, ok := entries[0].Message.(*criu_fs.FsEntry)
	if !ok {
		return "", "", fmt.Errorf("unexpected fs entry type for PID %d", pid)
	}
	umask := ""
	if fsEntry.Umask != nil {
		umask = fmt.Sprintf("%04o", fsEntry.GetUmask())
	}

	files, err := critEntries(filepath.Join(criuDir, "files.img"), func(c crit.Critter) (*crit.CriuImage, error) {
		return c.Decode(&fdinfo.FileEntry{})
	})
	if err != nil {
		return "", "", err
	}
	for _, entry := range files {
		file, ok := entry.Message.(*fdinfo.FileEntry)
		if !ok {
			return "", "", fmt.Errorf("unexpected files entry type")
		}
		if file.GetId() == fsEntry.GetCwdId() && file.GetReg() != nil {
			return file.GetReg().GetName(), umask, nil
		}
	}
	return "", "", fmt.Errorf("working directory of PID %d (file id %d) is not in files.img", pid, fsEntry.GetCwdId())
}

// exploreShell finds the session shell among processes, the tree read from
// criuDir, and returns its state. The shell is the session leader of a pty
// whose parent is the root of the tree (bash_init). It returns nil when the
// checkpoint has no such process.
func exploreShell(criuDir string, processes map[uint32]ProcInfo) (*shellState, error) {
	ptys, err := readPtys(criuDir)
	if err != nil {
		return nil, err
	}
	var shellPty *ptyInfo
	for _, p := range ptys {
		leader, ok := processes[p.sid]
		if !ok {
			continue
		}
		if parent, ok := processes[leader.PPID]; !ok || parent.PPID != 0 {
			continue
		}
		// bash_init opens its pty first; a pty opened later by a child of
		// bash_init would have a higher index.
		if shellPty == nil || p.index < shellPty.index {
			shellPty = p
		}
	}
	if shellPty == nil {
		return nil, nil
	}

	shell := processes[shellPty.sid]
	state := &shellState{
		pid:        shell.PID,
		foreground: []ShellProc{},
		size:       TTYSize{Rows: shellPty.winsize.GetWsRow(), Cols: shellPty.winsize.GetWsCol()},
		termios:    shellPty.termios,
	}
	if state.cwd, state.umask, err = readCwdAndUmask(criuDir, shell.PID); err != nil {
		return nil, err
	}
	if shellPty.pgrp != shell.PGID {
		for _, p := range processes {
			if p.PGID == shellPty.pgrp {
				state.foreground = append(state.foreground, ShellProc{PID: p.PID, Comm: p.Comm})
			}
		}
		sort.Slice(state.foreground, func(i, j int) bool {
			return state.foreground[i].PID < state.foreground[j].PID
		})
	}
	return state, nil
}

// ttyNamedFlags are the c_lflag bits reported under their own name.
const ttyNamedFlags = unix.ECHO | unix.ICANON | unix.ISIG

func termiosValue(t *tty.TermiosEntry) TTYTermios {
	cc := make([]string, 0, len(t.GetCCc()))
	for _, word := range t.GetCCc() {
		cc = append(cc, fmt.Sprintf("%08x", word))
	}
	return TTYTermios{
		Iflag: fmt.Sprintf("%#x", t.GetCIflag()),
		Oflag: fmt.Sprintf("%#x", t.GetCOflag()),
		Cflag: fmt.Sprintf("%#x", t.GetCCflag()),
		Lflag: fmt.Sprintf("%#x", t.GetCLflag()),
		Line:  t.GetCLine(),
		Cc:    strings.Join(cc, " "),
	}
}

// sameUnnamedTermios compares everything in two terminal settings but the
// flags in ttyNamedFlags.
func sameUnnamedTermios(left, right *tty.TermiosEntry) bool {
	return left.GetCIflag() == right.GetCIflag() &&
		left.GetCOflag() == right.GetCOflag() &&
		left.GetCCflag() == right.GetCCflag() &&
		left.GetCLflag()&^ttyNamedFlags == right.GetCLflag()&^ttyNamedFlags &&
		left.GetCLine() == right.GetCLine() &&
		slices.Equal(left.GetCCc(), right.GetCCc())
}

// diffShell lists what differs between the shell of two checkpoints. Either
// side is nil when its checkpoint has no session shell.
func diffShell(left, right *shellState) []ShellChange {
	changes := []ShellChange{}
	if left == nil && right == nil {
		return changes
	}
	if left == nil || right == nil {
		var from, to any
		if left != nil {
			from = left.pid
		}
		if right != nil {
			to = right.pid
		}
		return append(changes, ShellChange{Kind: ShellKindShell, From: from, To: to})
	}

	if left.pid != right.pid {
		changes = append(changes, ShellChange{Kind: ShellKindShell, From: left.pid, To: right.pid})
	}
	if left.cwd != right.cwd {
		changes = append(changes, ShellChange{Kind: ShellKindCwd, From: left.cwd, To: right.cwd})
	}
	if left.umask != right.umask {
		changes = append(changes, ShellChange{Kind: ShellKindUmask, From: left.umask, To: right.umask})
	}
	if !slices.Equal(left.foreground, right.foreground) {
		changes = append(changes, ShellChange{Kind: ShellKindForeground, From: left.foreground, To: right.foreground})
	}
	if left.size != right.size {
		changes = append(changes, ShellChange{Kind: ShellKindTTY, Name: TTYNameSize, From: left.size, To: right.size})
	}
	for _, flag := range []struct {
		name string
		bit  uint32
	}{
		{TTYNameEcho, unix.ECHO},
		{TTYNameCanonical, unix.ICANON},
		{TTYNameSignals, unix.ISIG},
	} {
		before := left.termios.GetCLflag()&flag.bit != 0
		after := right.termios.GetCLflag()&flag.bit != 0
		if before != after {
			changes = append(changes, ShellChange{Kind: ShellKindTTY, Name: flag.name, From: before, To: after})
		}
	}
	if !sameUnnamedTermios(left.termios, right.termios) {
		changes = append(changes, ShellChange{
			Kind: ShellKindTTY, Name: TTYNameTermios,
			From: termiosValue(left.termios), To: termiosValue(right.termios),
		})
	}
	return changes
}
