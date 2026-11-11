package waypoint

// Process state of a checkpoint, decoded from its CRIU images: the process
// tree with each process's argv, initial environment, executable, cwd,
// credentials and open file descriptors, and the sockets those descriptors
// hold. `waypoint state` prints it; `waypoint diff` compares two of them.
//
// Processes are identified across checkpoints by a structural key, not by
// PID: the argv of each ancestor and of the process itself, plus an ordinal
// among siblings with the same argv. CRIU restores the same PIDs along one
// lineage, but PIDs say nothing across branches or sessions.
//
// Waypoint's own plumbing is marked internal: the pstree root (bash_init),
// descriptors on files under /.waypoint (the completion FIFO) and the shell
// control socket under the session directory. Diffs leave it out.
//
// Not covered: memory contents beyond argv and environment, shell variables
// (they live in the shell's heap), and kernel state that the images keep
// outside the process records (namespaces, mounts, timers).

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// ProcessState is the process side of one checkpoint.
type ProcessState struct {
	Session    string          `json:"session"`
	Checkpoint string          `json:"checkpoint"`
	Processes  []ProcessRecord `json:"processes"`
	Sockets    []SocketRecord  `json:"sockets"`
	// Unavailable lists what could not be read, such as the argv of a
	// process whose stack pages are compressed.
	Unavailable []string `json:"unavailable,omitempty"`
}

type ProcessRecord struct {
	Key      string     `json:"key"`
	PID      int        `json:"pid"`
	PPID     int        `json:"ppid"`
	PGID     int        `json:"pgid"`
	SID      int        `json:"sid"`
	Internal bool       `json:"internal,omitempty"`
	Comm     string     `json:"comm"`
	Exe      string     `json:"exe,omitempty"`
	Argv     []string   `json:"argv"`
	Env      []string   `json:"env"` // initial environment, sorted
	Cwd      string     `json:"cwd,omitempty"`
	Root     string     `json:"root,omitempty"`
	UID      uint32     `json:"uid"`
	GID      uint32     `json:"gid"`
	EUID     uint32     `json:"euid"`
	EGID     uint32     `json:"egid"`
	State    string     `json:"state"`
	Threads  int        `json:"threads"`
	FDs      []FDRecord `json:"fds"`
}

// FDRecord is one open descriptor. Target is the path of a file or FIFO, or
// the address of a socket; it is empty for anonymous objects (pipes, ttys,
// eventfds), whose identities do not carry over between checkpoints.
type FDRecord struct {
	FD       int    `json:"fd"`
	Kind     string `json:"kind"`
	Target   string `json:"target,omitempty"`
	Access   string `json:"access,omitempty"` // r, w, rw, with "a" for O_APPEND
	Internal bool   `json:"internal,omitempty"`
}

// SocketRecord is an inet socket, or a unix socket with a name, held by at
// least one process. Owners are process keys.
type SocketRecord struct {
	Proto  string   `json:"proto"`
	Local  string   `json:"local"`
	Remote string   `json:"remote,omitempty"`
	State  string   `json:"state"`
	Owners []string `json:"owners"`
}

func (s SocketRecord) identity() string {
	return s.Proto + " " + s.Local + " " + s.Remote + " " + s.State
}

func (r FDRecord) identity() string {
	return r.Kind + " " + r.Target + " " + r.Access
}

// fileInfo is what files.img says about one file object.
type fileInfo struct {
	kind     string
	target   string
	access   string
	internal bool
	socket   *SocketRecord // inet or named unix socket
}

// fd_types (images/fdinfo.proto).
var criuFdTypes = map[uint64]string{
	1: "file", 2: "pipe", 3: "fifo", 4: "inet", 5: "unix", 6: "eventfd", 7: "eventpoll",
	8: "inotify", 9: "signalfd", 10: "packet", 11: "tty", 12: "fanotify", 13: "netlink",
	14: "ns", 15: "tun", 16: "external", 17: "timerfd", 18: "memfd", 19: "bpfmap", 20: "pidfd",
}

// Linux TCP states, which CRIU also uses for unix sockets.
var tcpStates = map[uint64]string{
	1: "established", 2: "syn_sent", 3: "syn_recv", 4: "fin_wait1", 5: "fin_wait2", 6: "time_wait",
	7: "close", 8: "close_wait", 9: "last_ack", 10: "listen", 11: "closing",
}

var taskStates = map[uint64]string{1: "alive", 2: "dead", 3: "stopped", 6: "zombie"}

// CheckpointProcessState decodes the process state of a ready checkpoint.
func (m *Manager) CheckpointProcessState(checkpointID string) (*ProcessState, error) {
	if err := validateCheckpointID(checkpointID); err != nil {
		return nil, err
	}
	md, err := m.loadMetadata(checkpointID)
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata for checkpoint %s: %w", checkpointID, err)
	}
	if md.Status != CheckpointStatusReady {
		return nil, fmt.Errorf("checkpoint %s is not ready (status: %s)", checkpointID, md.Status)
	}
	// Hold the images lock shared, as a restore does, so a tmpfs flush
	// cannot repoint the images while they are read.
	if unlock, err := lockImages(m.checkpointDir(checkpointID), false); err == nil {
		defer unlock()
	}
	st, err := decodeProcessState(m.checkpointCriuDir(checkpointID), m.baseDir)
	if err != nil {
		return nil, fmt.Errorf("checkpoint %s: %w", checkpointID, err)
	}
	st.Session = m.sessionID
	st.Checkpoint = checkpointID
	return st, nil
}

// decodeProcessState reads an image set. sessionDir is the session's base
// directory, under which the shell control socket lives in every fork.
func decodeProcessState(dir, sessionDir string) (*ProcessState, error) {
	st := &ProcessState{Processes: []ProcessRecord{}, Sockets: []SocketRecord{}}
	tree, err := readCriuImage(filepath.Join(dir, "pstree.img"), criuPstreeMagic)
	if err != nil {
		return nil, err
	}
	files, err := decodeFiles(filepath.Join(dir, "files.img"), sessionDir)
	if err != nil {
		return nil, err
	}
	note := func(format string, args ...any) {
		st.Unavailable = append(st.Unavailable, fmt.Sprintf(format, args...))
	}

	for i, e := range tree {
		pid, _ := e.uint(1)
		ppid, _ := e.uint(2)
		pgid, _ := e.uint(3)
		sid, _ := e.uint(4)
		threads, err := e.uints(5)
		if err != nil {
			return nil, fmt.Errorf("pstree: %w", err)
		}
		p := ProcessRecord{PID: int(pid), PPID: int(ppid), PGID: int(pgid), SID: int(sid), Threads: len(threads),
			Argv: []string{}, Env: []string{}, FDs: []FDRecord{}}
		if err := decodeProcess(dir, &p, files, note); err != nil {
			return nil, fmt.Errorf("pid %d: %w", pid, err)
		}
		// The root of the dumped tree is bash_init, Waypoint's supervisor.
		p.Internal = i == 0
		st.Processes = append(st.Processes, p)
	}
	assignProcessKeys(st.Processes)

	// Sockets, with the processes that hold them.
	byID := map[string]*SocketRecord{}
	for _, p := range st.Processes {
		if p.Internal {
			continue
		}
		for _, fd := range p.FDs {
			if fd.Internal {
				continue
			}
			info := files.bySocketFD(p.PID, fd.FD)
			if info == nil || info.socket == nil {
				continue
			}
			id := info.socket.identity()
			rec, ok := byID[id]
			if !ok {
				rec = &SocketRecord{Proto: info.socket.Proto, Local: info.socket.Local, Remote: info.socket.Remote, State: info.socket.State}
				byID[id] = rec
			}
			if len(rec.Owners) == 0 || rec.Owners[len(rec.Owners)-1] != p.Key {
				rec.Owners = append(rec.Owners, p.Key)
			}
		}
	}
	for _, rec := range byID {
		sort.Strings(rec.Owners)
		st.Sockets = append(st.Sockets, *rec)
	}
	sort.Slice(st.Sockets, func(i, j int) bool { return st.Sockets[i].identity() < st.Sockets[j].identity() })
	return st, nil
}

// decodeProcess fills in one process from its per-task images.
func decodeProcess(dir string, p *ProcessRecord, files *fileTable, note func(string, ...any)) error {
	pid := strconv.Itoa(p.PID)
	core, err := readCriuImageOne(filepath.Join(dir, "core-"+pid+".img"), criuCoreMagic)
	if err != nil {
		return err
	}
	if tc, ok, err := core.msg(3); err != nil {
		return fmt.Errorf("core: %w", err)
	} else if ok {
		state, _ := tc.uint(1)
		p.State = taskStates[state]
		if p.State == "" {
			p.State = "unknown-" + strconv.FormatUint(state, 10)
		}
		p.Comm = tc.str(6)
	}
	if th, ok, err := core.msg(5); err != nil {
		return fmt.Errorf("core: %w", err)
	} else if ok {
		if creds, ok, err := th.msg(10); err != nil {
			return fmt.Errorf("creds: %w", err)
		} else if ok {
			uid, _ := creds.uint(1)
			gid, _ := creds.uint(2)
			euid, _ := creds.uint(3)
			egid, _ := creds.uint(4)
			p.UID, p.GID, p.EUID, p.EGID = uint32(uid), uint32(gid), uint32(euid), uint32(egid)
		}
	}
	if p.State == "zombie" || p.State == "dead" {
		return nil // no mm, fs or files
	}

	// Descriptors: ids-<pid> names the fd table (files_id); older images
	// keep the ids in the core entry.
	ids, err := readCriuImageOne(filepath.Join(dir, "ids-"+pid+".img"), criuIdsMagic)
	if os.IsNotExist(err) {
		var ok bool
		if ids, ok, err = core.msg(4); err == nil && !ok {
			err = fmt.Errorf("no task ids")
		}
	}
	if err != nil {
		return fmt.Errorf("ids: %w", err)
	}
	filesID, _ := ids.uint(2)
	fdinfo, err := readCriuImage(filepath.Join(dir, "fdinfo-"+strconv.FormatUint(filesID, 10)+".img"), criuFdinfoMagic)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, e := range fdinfo {
		id, _ := e.uint(1)
		fd, _ := e.uint(4)
		info := files.get(id)
		p.FDs = append(p.FDs, FDRecord{FD: int(fd), Kind: info.kind, Target: info.target, Access: info.access, Internal: info.internal})
		files.noteSocketFD(p.PID, int(fd), id)
	}
	sort.Slice(p.FDs, func(i, j int) bool { return p.FDs[i].FD < p.FDs[j].FD })

	if fs, err := readCriuImageOne(filepath.Join(dir, "fs-"+pid+".img"), criuFsMagic); err == nil {
		cwd, _ := fs.uint(1)
		root, _ := fs.uint(2)
		p.Cwd = files.get(cwd).target
		p.Root = files.get(root).target
	} else if !os.IsNotExist(err) {
		return err
	}

	mm, err := readCriuImageOne(filepath.Join(dir, "mm-"+pid+".img"), criuMmMagic)
	if os.IsNotExist(err) {
		return nil // e.g. a kernel thread
	}
	if err != nil {
		return err
	}
	if exe, ok := mm.uint(12); ok {
		p.Exe = files.get(exe).target
	}
	argStart, _ := mm.uint(8)
	argEnd, _ := mm.uint(9)
	envStart, _ := mm.uint(10)
	envEnd, _ := mm.uint(11)
	pages, err := openPageSource(dir, p.PID)
	if err != nil {
		note("pid %d: argv and environment: %v", p.PID, err)
		return nil
	}
	args, argErr := pages.readRange(argStart, argEnd)
	if argErr != nil {
		note("pid %d: argv: %v", p.PID, argErr)
	}
	env, envErr := pages.readRange(envStart, envEnd)
	if envErr != nil {
		note("pid %d: environment: %v", p.PID, envErr)
	}
	if argErr == nil {
		var title bool
		p.Argv, env, title = argvAndEnv(args, env, envErr == nil && envStart == argEnd)
		if title {
			note("pid %d: environment: overwritten by the process title", p.PID)
		}
	}
	if envErr == nil {
		p.Env = splitNul(env)
		sort.Strings(p.Env)
	}
	return nil
}

// argvAndEnv splits the argument area into argv. A process that sets its
// title (nginx, postgres, sshd) overwrites the area in place, so its last
// byte is no longer NUL; the title may also run on into the environment area
// right after it. Like the kernel's /proc/<pid>/cmdline, argv is then the
// one string up to the first NUL, and the environment bytes the title used
// are dropped. contiguous says env starts where args end.
func argvAndEnv(args, env []byte, contiguous bool) (argv []string, rest []byte, title bool) {
	if len(args) == 0 || args[len(args)-1] == 0 {
		return splitNul(args), env, false
	}
	s := args
	rest = env
	if contiguous {
		n := bytes.IndexByte(env, 0)
		if n < 0 {
			n = len(env)
		}
		s = append(append([]byte{}, args...), env[:n]...)
		rest = env[n:]
		title = n > 0
	}
	if i := bytes.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	if len(s) == 0 {
		return []string{}, rest, title
	}
	return []string{string(s)}, rest, title
}

func splitNul(b []byte) []string {
	out := []string{}
	for _, s := range strings.Split(string(b), "\x00") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// assignProcessKeys gives every process its structural key: the parent's key,
// " > ", its own label (argv, or [comm] when argv is unknown), and "#n" for
// the n-th sibling (by PID) with the same label.
func assignProcessKeys(procs []ProcessRecord) {
	byPID := map[int]int{}
	for i := range procs {
		byPID[procs[i].PID] = i
	}
	children := map[int][]int{}
	var roots []int
	for i, p := range procs {
		if _, ok := byPID[p.PPID]; ok && p.PPID != p.PID {
			children[p.PPID] = append(children[p.PPID], i)
		} else {
			roots = append(roots, i)
		}
	}
	var assign func(prefix string, idx []int)
	assign = func(prefix string, idx []int) {
		sort.Slice(idx, func(a, b int) bool { return procs[idx[a]].PID < procs[idx[b]].PID })
		seen := map[string]int{}
		for _, i := range idx {
			label := processLabel(procs[i])
			seen[label]++
			if seen[label] > 1 {
				label += "#" + strconv.Itoa(seen[label])
			}
			key := label
			if prefix != "" {
				key = prefix + " > " + label
			}
			procs[i].Key = key
			assign(key, children[procs[i].PID])
		}
	}
	assign("", roots)
}

func processLabel(p ProcessRecord) string {
	if p.Internal {
		// bash_init's argv can name the session; keep keys comparable
		// across sessions.
		return "[waypoint]"
	}
	if len(p.Argv) > 0 {
		return strings.Join(p.Argv, " ")
	}
	return "[" + p.Comm + "]"
}

// --- files.img ---

type fileTable struct {
	byID map[uint64]fileInfo
	// socket fds of each process, to attribute sockets to owners
	sockFD map[[2]int]uint64
}

func (t *fileTable) get(id uint64) fileInfo {
	if info, ok := t.byID[id]; ok {
		return info
	}
	return fileInfo{kind: "unknown"}
}

func (t *fileTable) noteSocketFD(pid, fd int, id uint64) {
	if t.byID[id].socket != nil {
		t.sockFD[[2]int{pid, fd}] = id
	}
}

func (t *fileTable) bySocketFD(pid, fd int) *fileInfo {
	id, ok := t.sockFD[[2]int{pid, fd}]
	if !ok {
		return nil
	}
	info := t.byID[id]
	return &info
}

// decodeFiles reads files.img into descriptions of each file object.
func decodeFiles(path, sessionDir string) (*fileTable, error) {
	entries, err := readCriuImage(path, criuFilesMagic)
	if err != nil {
		return nil, err
	}
	t := &fileTable{byID: map[uint64]fileInfo{}, sockFD: map[[2]int]uint64{}}
	internalPath := func(p string) bool {
		return isUnder(p, "/.waypoint") || (sessionDir != "" && isUnder(p, sessionDir))
	}
	regNames := map[uint64]string{}
	var fifos []pbMsg
	for _, e := range entries {
		typ, _ := e.uint(1)
		id, _ := e.uint(2)
		info := fileInfo{kind: criuFdTypes[typ]}
		if info.kind == "" {
			info.kind = "type-" + strconv.FormatUint(typ, 10)
		}
		switch typ {
		case 1: // REG
			reg, _, err := e.msg(3)
			if err != nil {
				return nil, fmt.Errorf("files.img: reg: %w", err)
			}
			info.target = reg.str(6)
			flags, _ := reg.uint(2)
			info.access = accessMode(flags)
			regNames[id] = info.target
		case 3: // FIFO: the path is in the REG entry regf_id
			if f, ok, err := e.msg(17); err != nil {
				return nil, fmt.Errorf("files.img: fifo: %w", err)
			} else if ok {
				fifos = append(fifos, f)
			}
		case 4: // INETSK
			isk, _, err := e.msg(4)
			if err != nil {
				return nil, fmt.Errorf("files.img: inet socket: %w", err)
			}
			s, err := inetSocket(isk)
			if err != nil {
				return nil, fmt.Errorf("files.img: inet socket %d: %w", id, err)
			}
			info.kind, info.target, info.socket = s.Proto, s.Local+" "+s.State, s
			if s.Remote != "" {
				info.target = s.Local + " -> " + s.Remote + " " + s.State
			}
		case 5: // UNIXSK
			usk, _, err := e.msg(16)
			if err != nil {
				return nil, fmt.Errorf("files.img: unix socket: %w", err)
			}
			name, _ := usk.bytes(11)
			state, _ := usk.uint(4)
			info.target = unixName(name, usk.str(14))
			if info.target != "" {
				info.socket = &SocketRecord{Proto: "unix", Local: info.target, State: tcpStates[state]}
				info.target += " " + tcpStates[state]
			}
			info.internal = internalPath(unixName(name, usk.str(14)))
		}
		if typ == 1 {
			info.internal = internalPath(info.target)
		}
		t.byID[id] = info
	}
	for _, f := range fifos {
		id, _ := f.uint(1)
		info := t.byID[id]
		if regf, ok := f.uint(3); ok {
			info.target = regNames[regf]
			info.access = t.byID[regf].access
			info.internal = internalPath(info.target)
		}
		t.byID[id] = info
	}
	return t, nil
}

func accessMode(flags uint64) string {
	var s string
	switch flags & 3 { // O_ACCMODE
	case 0:
		s = "r"
	case 1:
		s = "w"
	default:
		s = "rw"
	}
	if flags&0o2000 != 0 { // O_APPEND
		s += "a"
	}
	return s
}

func unixName(name []byte, dir string) string {
	if len(name) == 0 {
		return ""
	}
	if name[0] == 0 {
		return "@" + string(name[1:]) // abstract
	}
	n := string(name)
	if !strings.HasPrefix(n, "/") && dir != "" {
		n = filepath.Join(dir, n)
	}
	return n
}

func inetSocket(isk pbMsg) (*SocketRecord, error) {
	family, _ := isk.uint(3)
	typ, _ := isk.uint(4)
	state, _ := isk.uint(6)
	sport, _ := isk.uint(7)
	dport, _ := isk.uint(8)
	src, err := isk.uints(11)
	if err != nil {
		return nil, err
	}
	dst, err := isk.uints(12)
	if err != nil {
		return nil, err
	}
	proto := "tcp"
	if typ == 2 { // SOCK_DGRAM
		proto = "udp"
	} else if typ != 1 {
		proto = "inet-type-" + strconv.FormatUint(typ, 10)
	}
	if family == 10 { // AF_INET6
		proto += "6"
	}
	s := &SocketRecord{Proto: proto, Local: hostPort(src, sport), State: tcpStates[state]}
	if s.State == "" {
		s.State = "state-" + strconv.FormatUint(state, 10)
	}
	// A remote end exists for connected sockets only: not for listeners
	// and not for unconnected UDP (state close).
	if state != 10 && state != 7 {
		s.Remote = hostPort(dst, dport)
	}
	return s, nil
}

// hostPort formats an address that CRIU stores as u32 words holding the
// address bytes in network order.
func hostPort(words []uint64, port uint64) string {
	if len(words) != 1 && len(words) != 4 {
		return fmt.Sprintf("?:%d", port)
	}
	b := make([]byte, 4*len(words))
	for i, w := range words {
		binary.LittleEndian.PutUint32(b[4*i:], uint32(w))
	}
	return net.JoinHostPort(net.IP(b).String(), strconv.FormatUint(port, 10))
}

// isUnder reports whether p is dir or below it.
func isUnder(p, dir string) bool {
	dir = strings.TrimSuffix(dir, "/")
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// --- diff ---

// ProcessDiff compares two process states. Internal processes and
// descriptors are left out.
type ProcessDiff struct {
	Added       []ProcessRecord `json:"added"`
	Removed     []ProcessRecord `json:"removed"`
	Changed     []ProcessChange `json:"changed"`
	Sockets     SocketDiff      `json:"sockets"`
	Unavailable []string        `json:"unavailable,omitempty"`
}

type ProcessChange struct {
	Key     string        `json:"key"`
	FromPID int           `json:"from_pid"`
	ToPID   int           `json:"to_pid"`
	Fields  []FieldChange `json:"fields,omitempty"`
	Env     *EnvChange    `json:"env,omitempty"`
	FDs     *FDChange     `json:"fds,omitempty"`
}

type FieldChange struct {
	Field string `json:"field"`
	From  any    `json:"from"`
	To    any    `json:"to"`
}

type EnvChange struct {
	Added   []string    `json:"added"`
	Removed []string    `json:"removed"`
	Changed []EnvUpdate `json:"changed"`
}

type EnvUpdate struct {
	Name string `json:"name"`
	From string `json:"from"`
	To   string `json:"to"`
}

type FDChange struct {
	Added   []FDRecord `json:"added"`
	Removed []FDRecord `json:"removed"`
	Changed []FDUpdate `json:"changed"`
}

type FDUpdate struct {
	FD   int      `json:"fd"`
	From FDRecord `json:"from"`
	To   FDRecord `json:"to"`
}

type SocketDiff struct {
	Added   []SocketRecord `json:"added"`
	Removed []SocketRecord `json:"removed"`
	// Changed lists sockets present on both sides whose owners differ, such
	// as a port that another server now listens on.
	Changed []SocketUpdate `json:"changed"`
}

type SocketUpdate struct {
	Proto      string   `json:"proto"`
	Local      string   `json:"local"`
	Remote     string   `json:"remote,omitempty"`
	State      string   `json:"state"`
	FromOwners []string `json:"from_owners"`
	ToOwners   []string `json:"to_owners"`
}

func diffProcessStates(a, b *ProcessState) *ProcessDiff {
	d := &ProcessDiff{Added: []ProcessRecord{}, Removed: []ProcessRecord{}, Changed: []ProcessChange{},
		Sockets: SocketDiff{Added: []SocketRecord{}, Removed: []SocketRecord{}, Changed: []SocketUpdate{}}}
	for _, u := range a.Unavailable {
		d.Unavailable = append(d.Unavailable, "from: "+u)
	}
	for _, u := range b.Unavailable {
		d.Unavailable = append(d.Unavailable, "to: "+u)
	}
	index := func(st *ProcessState) map[string]ProcessRecord {
		out := map[string]ProcessRecord{}
		for _, p := range st.Processes {
			if !p.Internal {
				p.FDs = externalFDs(p.FDs)
				out[p.Key] = p
			}
		}
		return out
	}
	pa, pb := index(a), index(b)
	for _, key := range sortedKeys(pa) {
		if _, ok := pb[key]; !ok {
			d.Removed = append(d.Removed, pa[key])
		}
	}
	for _, key := range sortedKeys(pb) {
		x, ok := pa[key]
		if !ok {
			d.Added = append(d.Added, pb[key])
			continue
		}
		if c := compareProcesses(x, pb[key]); c != nil {
			d.Changed = append(d.Changed, *c)
		}
	}

	sa, sb := map[string]SocketRecord{}, map[string]SocketRecord{}
	for _, s := range a.Sockets {
		sa[s.identity()] = s
	}
	for _, s := range b.Sockets {
		sb[s.identity()] = s
	}
	for _, id := range sortedKeys(sa) {
		if _, ok := sb[id]; !ok {
			d.Sockets.Removed = append(d.Sockets.Removed, sa[id])
		}
	}
	for _, id := range sortedKeys(sb) {
		x, ok := sa[id]
		if !ok {
			d.Sockets.Added = append(d.Sockets.Added, sb[id])
			continue
		}
		if y := sb[id]; !slices.Equal(x.Owners, y.Owners) {
			d.Sockets.Changed = append(d.Sockets.Changed, SocketUpdate{Proto: y.Proto, Local: y.Local, Remote: y.Remote,
				State: y.State, FromOwners: x.Owners, ToOwners: y.Owners})
		}
	}
	return d
}

func externalFDs(fds []FDRecord) []FDRecord {
	out := []FDRecord{}
	for _, fd := range fds {
		if !fd.Internal {
			out = append(out, fd)
		}
	}
	return out
}

func compareProcesses(a, b ProcessRecord) *ProcessChange {
	c := &ProcessChange{Key: a.Key, FromPID: a.PID, ToPID: b.PID}
	field := func(name string, x, y any) {
		if x != y {
			c.Fields = append(c.Fields, FieldChange{Field: name, From: x, To: y})
		}
	}
	field("comm", a.Comm, b.Comm)
	field("exe", a.Exe, b.Exe)
	field("cwd", a.Cwd, b.Cwd)
	field("root", a.Root, b.Root)
	field("state", a.State, b.State)
	field("uid", a.UID, b.UID)
	field("gid", a.GID, b.GID)
	field("euid", a.EUID, b.EUID)
	field("egid", a.EGID, b.EGID)

	ea, eb := envMap(a.Env), envMap(b.Env)
	env := &EnvChange{Added: []string{}, Removed: []string{}, Changed: []EnvUpdate{}}
	for _, k := range sortedKeys(ea) {
		if _, ok := eb[k]; !ok {
			env.Removed = append(env.Removed, k+"="+ea[k])
		}
	}
	for _, k := range sortedKeys(eb) {
		if v, ok := ea[k]; !ok {
			env.Added = append(env.Added, k+"="+eb[k])
		} else if v != eb[k] {
			env.Changed = append(env.Changed, EnvUpdate{Name: k, From: v, To: eb[k]})
		}
	}
	if len(env.Added)+len(env.Removed)+len(env.Changed) > 0 {
		c.Env = env
	}

	fa, fb := map[int]FDRecord{}, map[int]FDRecord{}
	for _, fd := range a.FDs {
		fa[fd.FD] = fd
	}
	for _, fd := range b.FDs {
		fb[fd.FD] = fd
	}
	fds := &FDChange{Added: []FDRecord{}, Removed: []FDRecord{}, Changed: []FDUpdate{}}
	for _, n := range sortedKeys(fa) {
		if _, ok := fb[n]; !ok {
			fds.Removed = append(fds.Removed, fa[n])
		}
	}
	for _, n := range sortedKeys(fb) {
		if x, ok := fa[n]; !ok {
			fds.Added = append(fds.Added, fb[n])
		} else if x.identity() != fb[n].identity() {
			fds.Changed = append(fds.Changed, FDUpdate{FD: n, From: x, To: fb[n]})
		}
	}
	if len(fds.Added)+len(fds.Removed)+len(fds.Changed) > 0 {
		c.FDs = fds
	}
	if len(c.Fields) == 0 && c.Env == nil && c.FDs == nil {
		return nil
	}
	return c
}

func envMap(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}

func sortedKeys[K int | string, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}
