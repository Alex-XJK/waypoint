package waypoint

import (
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestAssignProcessKeys(t *testing.T) {
	procs := []ProcessRecord{
		{PID: 1, Internal: true, Argv: []string{"/.waypoint/bash_init", "/s/abc/temp/shell_abc.sock"}},
		{PID: 10, PPID: 1, Argv: []string{"bash"}},
		{PID: 30, PPID: 10, Argv: []string{"worker"}},
		{PID: 20, PPID: 10, Argv: []string{"worker"}},
		{PID: 40, PPID: 10, Comm: "zombie"},
	}
	assignProcessKeys(procs)
	want := map[int]string{
		1:  "[waypoint]",
		10: "[waypoint] > bash",
		20: "[waypoint] > bash > worker",
		30: "[waypoint] > bash > worker#2",
		40: "[waypoint] > bash > [zombie]",
	}
	for _, p := range procs {
		if p.Key != want[p.PID] {
			t.Errorf("pid %d key %q, want %q", p.PID, p.Key, want[p.PID])
		}
	}
}

func TestInetSocketFormatting(t *testing.T) {
	// 127.0.0.1 as CRIU stores it: the address bytes in a u32 read little-endian.
	lo := uint64(0x0100007f)
	isk, _ := parsePB((&pbw{}).u(3, 2).u(4, 1).u(6, 10).u(7, 8080).u(8, 0).u(11, lo).u(12, 0).b)
	s, err := inetSocket(isk)
	if err != nil {
		t.Fatal(err)
	}
	if s.Proto != "tcp" || s.Local != "127.0.0.1:8080" || s.State != "listen" || s.Remote != "" {
		t.Fatalf("listener = %+v", s)
	}
	conn, _ := parsePB((&pbw{}).u(3, 10).u(4, 1).u(6, 1).u(7, 5000).u(8, 443).
		u(11, 0).u(11, 0).u(11, 0).u(11, 0x01000000).u(12, 0).u(12, 0).u(12, 0).u(12, 0x01000000).b)
	s, _ = inetSocket(conn)
	if s.Proto != "tcp6" || s.Local != "[::1]:5000" || s.Remote != "[::1]:443" || s.State != "established" {
		t.Fatalf("connection = %+v", s)
	}
}

func TestAccessMode(t *testing.T) {
	for flags, want := range map[uint64]string{0: "r", 1: "w", 2: "rw", 1 | 0o2000: "wa"} {
		if got := accessMode(flags); got != want {
			t.Errorf("accessMode(%#o) = %q, want %q", flags, got, want)
		}
	}
}

func TestDiffProcessStates(t *testing.T) {
	a := &ProcessState{
		Processes: []ProcessRecord{
			{Key: "[waypoint]", Internal: true},
			{Key: "[waypoint] > bash", PID: 10, Cwd: "/", Env: []string{"A=1", "B=2"},
				FDs: []FDRecord{{FD: 0, Kind: "tty"}, {FD: 7, Kind: "fifo", Target: "/.waypoint/exec.done", Internal: true}}},
			{Key: "[waypoint] > bash > old", PID: 11},
		},
	}
	b := &ProcessState{
		Processes: []ProcessRecord{
			{Key: "[waypoint]", Internal: true, Cwd: "/elsewhere"},
			{Key: "[waypoint] > bash", PID: 10, Cwd: "/data", Env: []string{"A=1", "B=3", "C=4"},
				FDs: []FDRecord{{FD: 0, Kind: "tty"}, {FD: 5, Kind: "file", Target: "/data/log", Access: "w"}}},
			{Key: "[waypoint] > bash > new", PID: 12},
		},
		Sockets: []SocketRecord{{Proto: "tcp", Local: "127.0.0.1:80", State: "listen", Owners: []string{"[waypoint] > bash > new"}}},
	}
	d := diffProcessStates(a, b)
	if len(d.Added) != 1 || d.Added[0].Key != "[waypoint] > bash > new" || len(d.Removed) != 1 || d.Removed[0].Key != "[waypoint] > bash > old" {
		t.Fatalf("added %v removed %v", d.Added, d.Removed)
	}
	if len(d.Changed) != 1 {
		t.Fatalf("changed = %+v (internal processes must not appear)", d.Changed)
	}
	c := d.Changed[0]
	if len(c.Fields) != 1 || c.Fields[0].Field != "cwd" {
		t.Fatalf("fields = %+v", c.Fields)
	}
	if len(c.Env.Added) != 1 || c.Env.Added[0] != "C=4" || len(c.Env.Changed) != 1 || c.Env.Changed[0].Name != "B" {
		t.Fatalf("env = %+v", c.Env)
	}
	if len(c.FDs.Added) != 1 || c.FDs.Added[0].FD != 5 || len(c.FDs.Removed) != 0 {
		t.Fatalf("fds = %+v (the internal fd 7 must not appear)", c.FDs)
	}
	if len(d.Sockets.Added) != 1 || len(d.Sockets.Removed) != 0 || len(d.Sockets.Changed) != 0 {
		t.Fatalf("sockets = %+v", d.Sockets)
	}
}

// A port that another server listens on is a changed socket, not an equal one.
func TestDiffSocketOwners(t *testing.T) {
	sock := func(owner string) []SocketRecord {
		return []SocketRecord{{Proto: "tcp", Local: "0.0.0.0:8080", State: "listen", Owners: []string{owner}}}
	}
	a := &ProcessState{Sockets: sock("[waypoint] > nginx: master process nginx")}
	b := &ProcessState{Sockets: sock("[waypoint] > bash > python3 -m http.server 8080")}
	d := diffProcessStates(a, b)
	if len(d.Sockets.Added) != 0 || len(d.Sockets.Removed) != 0 || len(d.Sockets.Changed) != 1 {
		t.Fatalf("sockets = %+v", d.Sockets)
	}
	c := d.Sockets.Changed[0]
	if c.Local != "0.0.0.0:8080" || c.FromOwners[0] != a.Sockets[0].Owners[0] || c.ToOwners[0] != b.Sockets[0].Owners[0] {
		t.Fatalf("changed = %+v", c)
	}
	if d := diffProcessStates(a, a); len(d.Sockets.Changed) != 0 {
		t.Fatalf("equal owners reported as changed: %+v", d.Sockets.Changed)
	}
}

func TestArgvAndEnv(t *testing.T) {
	cases := []struct {
		name       string
		args, env  string
		contiguous bool
		argv       []string
		rest       string
		title      bool
	}{
		{"plain", "python3\x00-m\x00http.server\x00", "A=1\x00B=2\x00", true,
			[]string{"python3", "-m", "http.server"}, "A=1\x00B=2\x00", false},
		// nginx: the title "nginx: master process nginx" is written over
		// argv ("nginx\0", 6 bytes) and runs on into the environment.
		{"title spills into env", "nginx:", " master process nginx\x00\x00\x00\x00", true,
			[]string{"nginx: master process nginx"}, "\x00\x00\x00\x00", true},
		{"title within args", "sshd: x\x00\x00\x00", "A=1\x00", true,
			[]string{"sshd: x"}, "A=1\x00", false},
		{"title fills args exactly", "worker", "\x00A=1\x00", true,
			[]string{"worker"}, "\x00A=1\x00", false},
		{"env elsewhere", "nginx:", "A=1\x00", false,
			[]string{"nginx:"}, "A=1\x00", false},
		{"empty", "", "A=1\x00", true, []string{}, "A=1\x00", false},
	}
	for _, c := range cases {
		argv, rest, title := argvAndEnv([]byte(c.args), []byte(c.env), c.contiguous)
		if !slices.Equal(argv, c.argv) || string(rest) != c.rest || title != c.title {
			t.Errorf("%s: argv %q rest %q title %v; want %q %q %v", c.name, argv, rest, title, c.argv, c.rest, c.title)
		}
	}
}

// layers creates a stack from top-first layer specs; each spec maps a path
// to file content, or "/" suffix for a directory.
func layers(t *testing.T, specs ...map[string]string) *fsView {
	t.Helper()
	v := &fsView{hashes: map[string]string{}}
	for i, spec := range specs {
		dir := filepath.Join(t.TempDir(), "l")
		os.MkdirAll(dir, 0o755)
		for p, content := range spec {
			host := filepath.Join(dir, p)
			if content == "/" {
				os.MkdirAll(host, 0o755)
				continue
			}
			os.MkdirAll(filepath.Dir(host), 0o755)
			os.WriteFile(host, []byte(content), 0o644)
		}
		id := "L" + string(rune('0'+len(specs)-1-i))
		if i == len(specs)-1 {
			id = originalLayerID
		}
		v.layers = append(v.layers, fsLayer{id: id, dir: dir})
	}
	return v
}

func paths(recs []FileRecord) map[string]string {
	out := map[string]string{}
	for _, r := range recs {
		out[r.Path] = r.Change
	}
	return out
}

func never(string) bool { return false }

func TestResolveShadowsAndHides(t *testing.T) {
	v := layers(t,
		map[string]string{"/a/f": "top", "/b": "file-over-dir"},
		map[string]string{"/a/f": "low", "/a/g": "g", "/b/x": "hidden"},
	)
	r, err := v.resolve("/a/f")
	if err != nil || r == nil || v.layers[r.layer].id != "L1" {
		t.Fatalf("/a/f resolved to %+v, %v", r, err)
	}
	if r, _ := v.resolve("/a/g"); r == nil || v.layers[r.layer].id != originalLayerID {
		t.Fatal("/a/g should come from the original")
	}
	if r, _ := v.resolve("/b/x"); r != nil {
		t.Fatal("/b/x is hidden by the file /b")
	}
}

func TestDiffFilesystemsCandidates(t *testing.T) {
	// Shared original; from adds nothing, to replaces /d (a directory with a
	// child) by a file and changes /a/f. /keep is never touched.
	orig := map[string]string{"/a/f": "1", "/d/child": "c", "/keep": "k"}
	from := layers(t, orig)
	to := layers(t, map[string]string{"/a/f": "2", "/d": "now a file"}, orig)
	to.layers[1].dir = from.layers[0].dir // the same original on both sides
	recs, err := diffFilesystems(from, to, 1, nil, never, never, patchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := paths(recs)
	want := map[string]string{"/a/f": "modified", "/d": "modified", "/d/child": "deleted"}
	if len(got) != len(want) {
		t.Fatalf("records %v, want %v", got, want)
	}
	for p, c := range want {
		if got[p] != c {
			t.Fatalf("records %v, want %v", got, want)
		}
	}
}

// Whiteouts and opaque directories need root (mknod 0:0, trusted xattrs).
func TestDiffFilesystemsWhiteoutAndOpaque(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root for whiteouts and trusted.overlay xattrs")
	}
	orig := map[string]string{"/gone/a": "a", "/redo/old": "o", "/same": "s"}
	from := layers(t, orig)
	to := layers(t, map[string]string{"/redo/new": "n"}, orig)
	to.layers[1].dir = from.layers[0].dir
	upper := to.layers[0].dir
	if err := unix.Mknod(filepath.Join(upper, "gone"), syscall.S_IFCHR, 0); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(filepath.Join(upper, "redo"), "trusted.overlay.opaque", []byte("y"), 0); err != nil {
		t.Fatal(err)
	}
	recs, err := diffFilesystems(from, to, 1, nil, never, never, patchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := paths(recs)
	want := map[string]string{"/gone": "deleted", "/gone/a": "deleted", "/redo/old": "deleted", "/redo/new": "added"}
	if len(got) != len(want) {
		t.Fatalf("records %v, want %v", got, want)
	}
	for p, c := range want {
		if got[p] != c {
			t.Fatalf("records %v, want %v", got, want)
		}
	}
}

func TestManifestDiffs(t *testing.T) {
	a := layers(t, map[string]string{"/x": "1", "/y": "same", "/only-a": "a"})
	b := layers(t, map[string]string{"/x": "2", "/y": "same", "/only-b/": "/"})
	ma, err := buildManifest(a.layers[0].dir)
	if err != nil {
		t.Fatal(err)
	}
	mb, err := buildManifest(b.layers[0].dir)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	pa, pb := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	os.WriteFile(pa, ma, 0o644)
	os.WriteFile(pb, mb, 0o644)
	got, err := manifestDiffs(pa, pb)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "/only-a" || got[1] != "/only-b" || got[2] != "/x" {
		t.Fatalf("manifest diffs = %v", got)
	}
}
