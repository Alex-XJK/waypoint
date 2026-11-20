package waypoint

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/checkpoint-restore/go-criu/v8/crit"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/fdinfo"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/fown"
	criu_fs "github.com/checkpoint-restore/go-criu/v8/crit/images/fs"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/regfile"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/tty"
	"golang.org/x/sys/unix"
)

// The terminal settings bash_init gives its pty, as CRIU dumps them.
const (
	testIflag = 0o2400
	testOflag = 0o5
	testCflag = 0o277
	testLflag = 0o104001 // ISIG on, ICANON and ECHO off
)

var testProcs = []ProcInfo{
	{PID: 1, PPID: 0, PGID: 1, SID: 1, Comm: "bash_init"},
	{PID: 11, PPID: 1, PGID: 11, SID: 11, Comm: "bash"},
}

// ptyFixture is one pty of tty-info.img. pgrp 0 means the session leader's group.
type ptyFixture struct {
	index      uint32
	sid, pgrp  uint32
	rows, cols uint32
	iflag      uint32
	lflag      uint32
	cc         []uint32
}

// fsFixture is the working directory and umask of one process.
type fsFixture struct {
	cwd   string
	umask uint32
}

func defaultPty(sid uint32) ptyFixture {
	return ptyFixture{sid: sid, rows: 40, cols: 120, iflag: testIflag, lflag: testLflag, cc: []uint32{0x157f1c03, 0x10004}}
}

// writeTTYInfoImage writes tty-info.img with a master and a slave entry per
// pty, as CRIU does: the session is on the master entry only.
func writeTTYInfoImage(t *testing.T, dir string, ptys ...ptyFixture) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	img := &crit.CriuImage{Magic: "TTY_INFO"}
	var id uint32
	for _, p := range ptys {
		pgrp := p.pgrp
		if pgrp == 0 {
			pgrp = p.sid
		}
		for _, side := range []struct{ sid, pgrp, rdev uint32 }{
			{p.sid, pgrp, 5<<8 | 2},  // /dev/ptmx
			{0, 0, 136<<8 | p.index}, // /dev/pts/<index>
		} {
			entryID, ptyType, no := id, tty.TtyType_PTY, false
			sid, grp, rdev, index := side.sid, side.pgrp, side.rdev, p.index
			id++
			img.Entries = append(img.Entries, &crit.CriuEntry{Message: &tty.TtyInfoEntry{
				Id: &entryID, Type: &ptyType, Locked: &no, Exclusive: &no, PacketMode: &no,
				Sid: &sid, Pgrp: &grp, Rdev: &rdev,
				Termios: testTermios(p.iflag, p.lflag, p.cc),
				Winsize: testWinsize(p.rows, p.cols),
				Pty:     &tty.TtyPtyEntry{Index: &index},
			}})
		}
	}
	writeCritImage(t, filepath.Join(dir, "tty-info.img"), img)
}

func testTermios(iflag, lflag uint32, cc []uint32) *tty.TermiosEntry {
	var oflag, cflag, zero uint32 = testOflag, testCflag, 0
	return &tty.TermiosEntry{
		CIflag: &iflag, COflag: &oflag, CCflag: &cflag, CLflag: &lflag,
		CLine: &zero, CIspeed: &zero, COspeed: &zero, CCc: cc,
	}
}

func testWinsize(rows, cols uint32) *tty.WinsizeEntry {
	var zero uint32
	return &tty.WinsizeEntry{WsRow: &rows, WsCol: &cols, WsXpixel: &zero, WsYpixel: &zero}
}

// writeFsImages writes fs-<pid>.img for each process and a files.img that
// holds their working directories.
func writeFsImages(t *testing.T, dir string, procs map[uint32]fsFixture) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := &crit.CriuImage{Magic: "FILES"}
	var zero32 uint32
	var zero64 uint64
	nextID := uint32(10)
	for pid, p := range procs {
		cwdID, rootID, umask := nextID, nextID+1, p.umask
		nextID += 2
		writeCritImage(t, filepath.Join(dir, fmt.Sprintf("fs-%d.img", pid)), &crit.CriuImage{
			Magic:   "FS",
			Entries: []*crit.CriuEntry{{Message: &criu_fs.FsEntry{CwdId: &cwdID, RootId: &rootID, Umask: &umask}}},
		})
		for id, name := range map[uint32]string{cwdID: p.cwd, rootID: "/"} {
			fileID, fileType, fileName := id, fdinfo.FdTypes_REG, name
			files.Entries = append(files.Entries, &crit.CriuEntry{Message: &fdinfo.FileEntry{
				Type: &fileType, Id: &fileID,
				Reg: &regfile.RegFileEntry{
					Id: &fileID, Flags: &zero32, Pos: &zero64, Name: &fileName,
					Fown: &fown.FownEntry{Uid: &zero32, Euid: &zero32, Signum: &zero32, PidType: &zero32, Pid: &zero32},
				},
			}})
		}
	}
	writeCritImage(t, filepath.Join(dir, "files.img"), files)
}

// writeShellCheckpoint writes the images of a checkpoint whose processes are
// procs and whose shell, pid 11, has the given pty, directory and umask.
func writeShellCheckpoint(t *testing.T, dir string, procs []ProcInfo, pty ptyFixture, shell fsFixture) {
	t.Helper()
	writeProcImages(t, dir, procs)
	writeTTYInfoImage(t, dir, pty)
	writeFsImages(t, dir, map[uint32]fsFixture{11: shell})
}

func exploreShellIn(t *testing.T, dir string) *shellState {
	t.Helper()
	processes, err := explorePsFlat(dir)
	if err != nil {
		t.Fatalf("explorePsFlat: %v", err)
	}
	state, err := exploreShell(dir, processes)
	if err != nil {
		t.Fatalf("exploreShell: %v", err)
	}
	return state
}

func TestExploreShellReadsCwdUmaskAndTTY(t *testing.T) {
	dir := t.TempDir()
	writeShellCheckpoint(t, dir, testProcs, defaultPty(11), fsFixture{cwd: "/data/sub dir", umask: 0o27})

	state := exploreShellIn(t, dir)
	if state == nil {
		t.Fatal("exploreShell found no shell")
	}
	if state.pid != 11 || state.cwd != "/data/sub dir" || state.umask != "0027" {
		t.Fatalf("shell = pid %d cwd %q umask %q, want pid 11 cwd \"/data/sub dir\" umask 0027", state.pid, state.cwd, state.umask)
	}
	if want := (TTYSize{Rows: 40, Cols: 120}); state.size != want {
		t.Fatalf("size = %+v, want %+v", state.size, want)
	}
	if state.termios.GetCLflag() != testLflag {
		t.Fatalf("lflag = %#o, want %#o", state.termios.GetCLflag(), testLflag)
	}
	if len(state.foreground) != 0 {
		t.Fatalf("foreground = %v, want none for an idle shell", state.foreground)
	}
}

func TestExploreShellListsForegroundJob(t *testing.T) {
	dir := t.TempDir()
	procs := append([]ProcInfo{
		{PID: 16, PPID: 11, PGID: 15, SID: 11, Comm: "cat"},
		{PID: 15, PPID: 11, PGID: 15, SID: 11, Comm: "sleep"},
		{PID: 14, PPID: 11, PGID: 14, SID: 11, Comm: "background"},
	}, testProcs...)
	pty := defaultPty(11)
	pty.pgrp = 15
	writeShellCheckpoint(t, dir, procs, pty, fsFixture{cwd: "/"})

	want := []ShellProc{{PID: 15, Comm: "sleep"}, {PID: 16, Comm: "cat"}}
	if state := exploreShellIn(t, dir); !reflect.DeepEqual(state.foreground, want) {
		t.Fatalf("foreground = %v, want %v", state.foreground, want)
	}
}

func TestExploreShellWithoutTerminalIsNil(t *testing.T) {
	dir := t.TempDir()
	writeProcImages(t, dir, testProcs)
	if state := exploreShellIn(t, dir); state != nil {
		t.Fatalf("exploreShell = %+v without tty-info.img, want nil", state)
	}
}

// A pty opened inside the session (tmux, script) is led by a process that is
// not a child of bash_init.
func TestExploreShellSkipsPtysOpenedInsideTheSession(t *testing.T) {
	procs := append([]ProcInfo{
		{PID: 20, PPID: 11, PGID: 20, SID: 20, Comm: "tmux"},
		{PID: 21, PPID: 20, PGID: 21, SID: 21, Comm: "bash"},
	}, testProcs...)
	inner := defaultPty(21)
	inner.index = 1

	dir := t.TempDir()
	writeProcImages(t, dir, procs)
	writeTTYInfoImage(t, dir, inner, defaultPty(11))
	writeFsImages(t, dir, map[uint32]fsFixture{11: {cwd: "/app"}, 21: {cwd: "/inner"}})
	if state := exploreShellIn(t, dir); state == nil || state.pid != 11 || state.cwd != "/app" {
		t.Fatalf("exploreShell = %+v, want the shell with pid 11 in /app", state)
	}

	dir = t.TempDir()
	writeProcImages(t, dir, procs)
	writeTTYInfoImage(t, dir, inner)
	writeFsImages(t, dir, map[uint32]fsFixture{21: {cwd: "/inner"}})
	if state := exploreShellIn(t, dir); state != nil {
		t.Fatalf("exploreShell = %+v with only an inner pty, want nil", state)
	}
}

func TestExploreShellFailsWithoutFsImage(t *testing.T) {
	dir := t.TempDir()
	writeShellCheckpoint(t, dir, testProcs, defaultPty(11), fsFixture{cwd: "/"})
	if err := os.Remove(filepath.Join(dir, "fs-11.img")); err != nil {
		t.Fatal(err)
	}
	processes, err := explorePsFlat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exploreShell(dir, processes); err == nil {
		t.Fatal("exploreShell succeeded without fs-11.img")
	}
}

func TestDiffShell(t *testing.T) {
	base := func() *shellState {
		return &shellState{
			pid: 11, cwd: "/app", umask: "0022", foreground: []ShellProc{},
			size:    TTYSize{Rows: 40, Cols: 120},
			termios: testTermios(testIflag, testLflag, []uint32{0x157f1c03}),
		}
	}
	with := func(change func(*shellState)) *shellState {
		s := base()
		change(s)
		return s
	}
	job := []ShellProc{{PID: 15, Comm: "sleep"}}
	rawTermios := TTYTermios{Iflag: "0x0", Oflag: "0x5", Cflag: "0xbf", Lflag: "0x8801", Cc: "157f1c03"}
	baseTermios := rawTermios
	baseTermios.Iflag = "0x500"

	cases := []struct {
		name        string
		left, right *shellState
		want        []ShellChange
	}{
		{"same", base(), base(), []ShellChange{}},
		{"no shell on either side", nil, nil, []ShellChange{}},
		{"shell appears", nil, base(), []ShellChange{{Kind: ShellKindShell, From: nil, To: uint32(11)}}},
		{"shell gone", base(), nil, []ShellChange{{Kind: ShellKindShell, From: uint32(11), To: nil}}},
		{"cd", base(), with(func(s *shellState) { s.cwd = "/tmp" }),
			[]ShellChange{{Kind: ShellKindCwd, From: "/app", To: "/tmp"}}},
		{"umask", base(), with(func(s *shellState) { s.umask = "0027" }),
			[]ShellChange{{Kind: ShellKindUmask, From: "0022", To: "0027"}}},
		{"command left running", base(), with(func(s *shellState) { s.foreground = job }),
			[]ShellChange{{Kind: ShellKindForeground, From: []ShellProc{}, To: job}}},
		{"resize", base(), with(func(s *shellState) { s.size = TTYSize{Rows: 50, Cols: 200} }),
			[]ShellChange{{Kind: ShellKindTTY, Name: TTYNameSize, From: TTYSize{Rows: 40, Cols: 120}, To: TTYSize{Rows: 50, Cols: 200}}}},
		{"echo on, signals off", base(),
			with(func(s *shellState) {
				s.termios = testTermios(testIflag, testLflag&^unix.ISIG|unix.ECHO, []uint32{0x157f1c03})
			}),
			[]ShellChange{
				{Kind: ShellKindTTY, Name: TTYNameEcho, From: false, To: true},
				{Kind: ShellKindTTY, Name: TTYNameSignals, From: true, To: false},
			}},
		{"canonical", base(),
			with(func(s *shellState) { s.termios = testTermios(testIflag, testLflag|unix.ICANON, []uint32{0x157f1c03}) }),
			[]ShellChange{{Kind: ShellKindTTY, Name: TTYNameCanonical, From: false, To: true}}},
		{"other terminal setting", base(),
			with(func(s *shellState) { s.termios = testTermios(0, testLflag, []uint32{0x157f1c03}) }),
			[]ShellChange{{Kind: ShellKindTTY, Name: TTYNameTermios, From: baseTermios, To: rawTermios}}},
		{"several", base(), with(func(s *shellState) { s.cwd, s.umask = "/", "0077" }),
			[]ShellChange{
				{Kind: ShellKindCwd, From: "/app", To: "/"},
				{Kind: ShellKindUmask, From: "0022", To: "0077"},
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := diffShell(tc.left, tc.right); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("diffShell = %+v, want %+v", got, tc.want)
			}
		})
	}
}
