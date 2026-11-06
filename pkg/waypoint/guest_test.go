package waypoint

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSetGuestOptionsValidates(t *testing.T) {
	m := &Manager{}
	for want, o := range map[string]GuestOptions{
		"KEY=VALUE":     {Env: []string{"NOEQUALS"}},
		"reserved":      {Env: []string{"WAYPOINT_HOSTNAME=x"}},
		"absolute":      {WorkDir: "app"},
		"hostname":      {Hostname: "bad name"},
		"hosts line":    {Hosts: []string{"10.0.0.1"}},
		"hosts line \"": {Hosts: []string{"10.0.0.1 a\n10.0.0.2 b"}},
	} {
		if err := m.SetGuestOptions(o); err == nil || !strings.Contains(err.Error(), strings.TrimSuffix(want, " \"")) {
			t.Fatalf("SetGuestOptions(%+v) = %v, want error containing %q", o, err, want)
		}
	}
	ok := GuestOptions{Env: []string{"A=1"}, WorkDir: "/app", Hostname: "db-1", Hosts: []string{"10.89.0.2 db"}}
	if err := m.SetGuestOptions(ok); err != nil || !reflect.DeepEqual(m.guest, ok) {
		t.Fatalf("SetGuestOptions(valid) = %v; kept %+v", err, m.guest)
	}
}

func TestSessionEnvLayersGuestEnvAndHostname(t *testing.T) {
	m := &Manager{
		imageEnv: []string{"PATH=/opt/bin", "MODE=image", "PAGER=less"},
		guest:    GuestOptions{Env: []string{"MODE=compose", "PAGER=more", "EXTRA=1"}, Hostname: "db"},
	}
	env := map[string]string{}
	for _, e := range m.sessionEnv() {
		k, v, _ := strings.Cut(e, "=")
		if _, dup := env[k]; dup {
			t.Fatalf("duplicate key %s", k)
		}
		env[k] = v
	}
	for k, want := range map[string]string{
		"PATH":              "/opt/bin", // image over base
		"MODE":              "compose",  // guest over image
		"EXTRA":             "1",
		"PAGER":             "cat", // the unattended floor still wins
		"WAYPOINT_HOSTNAME": "db",
	} {
		if env[k] != want {
			t.Fatalf("%s = %q, want %q", k, env[k], want)
		}
	}
	if _, set := (&Manager{}).sessionEnvMap()["WAYPOINT_HOSTNAME"]; set {
		t.Fatal("WAYPOINT_HOSTNAME set without a hostname")
	}
}

func (m *Manager) sessionEnvMap() map[string]string {
	env := map[string]string{}
	for _, e := range m.sessionEnv() {
		k, v, _ := strings.Cut(e, "=")
		env[k] = v
	}
	return env
}

func TestAppendGuestHostsStaysInsideRootfs(t *testing.T) {
	rootfs := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "etc", "hosts"), []byte("127.0.0.1 localhost\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := appendGuestHosts(rootfs, []string{"10.89.0.2 db", "10.89.0.3 web"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(rootfs, "etc", "hosts"))
	if string(data) != "127.0.0.1 localhost\n10.89.0.2 db\n10.89.0.3 web\n" {
		t.Fatalf("hosts = %q", data)
	}

	// An image whose /etc/hosts points outside the rootfs must not be followed.
	outside := filepath.Join(t.TempDir(), "host-file")
	if err := os.WriteFile(outside, []byte("host\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	evil := t.TempDir()
	if err := os.MkdirAll(filepath.Join(evil, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(evil, "etc", "hosts")); err != nil {
		t.Fatal(err)
	}
	if err := appendGuestHosts(evil, []string{"10.89.0.2 db"}); err == nil {
		t.Fatal("appendGuestHosts followed a symlink out of the rootfs")
	}
	if data, _ := os.ReadFile(outside); string(data) != "host\n" {
		t.Fatalf("file outside the rootfs changed: %q", data)
	}
}

func TestBuildahBudArgs(t *testing.T) {
	got := buildahBudArgs(BuildSpec{Context: "/ctx"}, "img:1")
	if !reflect.DeepEqual(got, []string{"bud", "-t", "img:1", "-f", "/ctx/Dockerfile", "/ctx"}) {
		t.Fatalf("default = %q", got)
	}
	got = buildahBudArgs(BuildSpec{Context: "/ctx", Dockerfile: "docker/Dev", Args: map[string]string{"B": "2", "A": "1"}, Target: "prod"}, "img:1")
	want := []string{"bud", "-t", "img:1", "-f", "/ctx/docker/Dev", "--build-arg", "A=1", "--build-arg", "B=2", "--target", "prod", "/ctx"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("full = %q\nwant %q", got, want)
	}
	if got := buildahBudArgs(BuildSpec{Context: "/ctx", Dockerfile: "/abs/Dockerfile"}, "t"); got[4] != "/abs/Dockerfile" {
		t.Fatalf("absolute dockerfile = %q", got)
	}
}
