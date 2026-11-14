package group

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Alex-XJK/waypoint/pkg/waypoint"
)

func svc(name string) Service { return Service{Name: name, Rootfs: "/rootfs"} }

func TestPlanAssignsAddressesInServiceOrder(t *testing.T) {
	recs, subnet, err := plan(Spec{Name: "shop", Services: []Service{svc("db"), svc("api"), svc("web")}})
	if err != nil {
		t.Fatal(err)
	}
	if subnet != DefaultSubnet {
		t.Fatalf("subnet = %s", subnet)
	}
	var got []string
	for _, r := range recs {
		got = append(got, r.Name+"="+r.Address)
	}
	want := []string{"db=10.89.0.2/24", "api=10.89.0.3/24", "web=10.89.0.4/24"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan = %v, want %v", got, want)
	}
	recs, subnet, err = plan(Spec{Name: "shop", Subnet: "172.30.8.0/22", Services: []Service{svc("a")}})
	if err != nil || subnet != "172.30.8.0/22" || recs[0].Address != "172.30.8.2/22" {
		t.Fatalf("custom subnet plan = %v %s %v", recs, subnet, err)
	}
}

func TestPlanRejectsBadSpecs(t *testing.T) {
	b := &waypoint.BuildSpec{Context: "/ctx"}
	for want, spec := range map[string]Spec{
		"invalid project":        {Name: "-x", Services: []Service{svc("a")}},
		"no services":            {Name: "p"},
		"duplicate":              {Name: "p", Services: []Service{svc("a"), svc("a")}},
		"longer than 63":         {Name: "p", Services: []Service{svc(strings.Repeat("a", 64))}},
		"invalid service":        {Name: "p", Services: []Service{svc("-a")}},
		"exactly one":            {Name: "p", Services: []Service{{Name: "a"}}},
		"exactly one of":         {Name: "p", Services: []Service{{Name: "a", Rootfs: "/r", Build: b}}},
		"invalid subnet":         {Name: "p", Subnet: "fd00::/64", Services: []Service{svc("a")}},
		"has room for":           {Name: "p", Subnet: "10.0.0.0/30", Services: []Service{svc("a"), svc("b")}},
		"room for 0":             {Name: "p", Subnet: "10.0.0.0/31", Services: []Service{svc("a")}},
		"is reserved":            {Name: "p", Services: []Service{{Name: "a", Rootfs: "/r", Env: []string{"WAYPOINT_X=1"}}}},
		"must be absolute":       {Name: "p", Services: []Service{{Name: "a", Rootfs: "/r", WorkDir: "app"}}},
		"invalid guest hostname": {Name: "p", Services: []Service{svc("web-")}},
	} {
		if _, _, err := plan(spec); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("plan(%+v) = %v, want error containing %q", spec, err, want)
		}
	}
}

func TestServiceArgvFollowsComposeSemantics(t *testing.T) {
	ep, cmd := []string{"/entry.sh"}, []string{"serve", "--port", "80"}
	cases := []struct {
		name string
		svc  Service
		want []string
	}{
		{"image defaults", Service{}, []string{"/entry.sh", "serve", "--port", "80"}},
		{"command overrides CMD", Service{Command: []string{"migrate"}}, []string{"/entry.sh", "migrate"}},
		{"entrypoint drops CMD", Service{Entrypoint: []string{"/bin/sh"}}, []string{"/bin/sh"}},
		{"both", Service{Entrypoint: []string{"python"}, Command: []string{"app.py"}}, []string{"python", "app.py"}},
		{"empty entrypoint", Service{Entrypoint: []string{}}, []string{}},
	}
	for _, c := range cases {
		if got := serviceArgv(c.svc, ep, cmd); !reflect.DeepEqual(got, c.want) && !(len(got) == 0 && len(c.want) == 0) {
			t.Fatalf("%s: serviceArgv = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestStartCommandQuotesAndBackgrounds(t *testing.T) {
	if startCommand(nil) != "" {
		t.Fatal("an empty command should start nothing")
	}
	got := startCommand([]string{"sh", "-c", "echo 'hi' $HOME"})
	want := `if command -v -- 'sh' >/dev/null; then 'sh' '-c' 'echo '\''hi'\'' $HOME' >` + serviceLog +
		` 2>&1 </dev/null & else echo 'sh: command not found or not executable'; false; fi`
	if got != want {
		t.Fatalf("startCommand = %s\nwant          %s", got, want)
	}
}

func TestHostsLines(t *testing.T) {
	got := hostsLines([]ServiceRecord{{Name: "db", Address: "10.89.0.2/24"}, {Name: "web", Address: "10.89.0.3/24"}})
	if !reflect.DeepEqual(got, []string{"10.89.0.2 db", "10.89.0.3 web"}) {
		t.Fatalf("hostsLines = %q", got)
	}
}

func TestReadAllSkipsTornRecords(t *testing.T) {
	dir := t.TempDir()
	if err := writeJSON(filepath.Join(dir, "b.json"), Replica{ID: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "a.json"), Replica{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := waypoint.AtomicWriteFile(filepath.Join(dir, "c.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readAll[Replica](dir)
	if err != nil || len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("readAll = %+v, %v", got, err)
	}
	if got, err := readAll[Replica](filepath.Join(dir, "missing")); err != nil || len(got) != 0 {
		t.Fatalf("readAll(missing) = %+v, %v", got, err)
	}
}

func TestNewReplicaIDIsValid(t *testing.T) {
	id, err := newReplicaID()
	if err != nil || waypoint.ValidateID("replica", id) != nil || !strings.HasPrefix(id, "r-") {
		t.Fatalf("newReplicaID = %q, %v", id, err)
	}
}

func TestMemberServiceRecognizesLiveMembersOnly(t *testing.T) {
	t.Setenv("WAYPOINT_SESSION_INFO_DIR", t.TempDir())
	rec := Record{Name: "shop", Services: []ServiceRecord{{Name: "db", SessionID: "s1"}, {Name: "web", SessionID: "s2"}}}
	if err := writeJSON(recordPath("shop"), rec); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ project, session, want string }{
		{"shop", "s2", "web"},
		{"shop", "s9", ""}, // a leftover of an earlier project named shop
		{"gone", "s1", ""}, // a removed project
	} {
		if got, err := MemberService(c.project, c.session); err != nil || got != c.want {
			t.Fatalf("MemberService(%s, %s) = %q, %v; want %q", c.project, c.session, got, err, c.want)
		}
	}
}

func TestIsProjectFabric(t *testing.T) {
	previous := waypoint.NetRuntimeDir
	waypoint.NetRuntimeDir = t.TempDir()
	t.Cleanup(func() { waypoint.NetRuntimeDir = previous })
	if err := os.MkdirAll(filepath.Dir(fabricPin("shop", "main")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fabricPin("shop", "main"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "pin")
	if err := os.Symlink(fabricPin("shop", "main"), link); err != nil {
		t.Fatal(err)
	}
	for pin, want := range map[string]bool{
		fabricPin("shop", "r-1"): true,
		link:                     true,
		filepath.Join(waypoint.NetRuntimeDir, "groups"):           false,
		filepath.Join(waypoint.NetRuntimeDir, "groupsx", "a"):     false,
		filepath.Join(waypoint.NetRuntimeDir, "s1", "main/netns"): false,
		"/var/run/netns/fabric":                                   false,
	} {
		if got := IsProjectFabric(pin); got != want {
			t.Fatalf("IsProjectFabric(%s) = %v, want %v", pin, got, want)
		}
	}
}

func TestClaimProjectIsAtomicAndExclusive(t *testing.T) {
	t.Setenv("WAYPOINT_SESSION_INFO_DIR", t.TempDir())
	rec := &Record{Name: "shop", Status: ProjectCreating}
	if err := claimProject(rec); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadRecord("shop"); err != nil || got.Status != ProjectCreating {
		t.Fatalf("LoadRecord after claim = %+v, %v", got, err)
	}
	if err := claimProject(rec); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second claim = %v, want already exists", err)
	}
	entries, err := os.ReadDir(groupsDir())
	if err != nil || len(entries) != 1 {
		t.Fatalf("groups dir holds %d entries (%v), want only the project", len(entries), err)
	}
}

func TestProjectLockKeepsExclusiveOutWhileSharedIsHeld(t *testing.T) {
	t.Setenv("WAYPOINT_SESSION_INFO_DIR", t.TempDir())
	if err := writeJSON(recordPath("shop"), Record{Name: "shop", Status: ProjectReady}); err != nil {
		t.Fatal(err)
	}
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go withProjectLock("shop", false, func() error { close(held); <-release; return nil })
	<-held
	if err := withProjectLock("shop", false, func() error { return nil }); err != nil {
		t.Fatalf("a second shared holder: %v", err)
	}
	go func() {
		_ = withProjectLock("shop", true, func() error { return nil })
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("the exclusive lock was granted while a shared one was held")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-done

	if err := withProjectLock("nope", true, func() error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("locking an unknown project = %v, want not found", err)
	}
	if _, err := os.Stat(projectLockPath("nope")); !os.IsNotExist(err) {
		t.Fatal("locking an unknown project left a lockfile")
	}
}
