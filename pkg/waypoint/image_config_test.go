package waypoint

import (
	"os/exec"
	"strings"
	"testing"
)

func TestInspectImageConfigParsesOCIConfig(t *testing.T) {
	const payload = `{"OCIv1":{"config":{"Env":["PATH=/opt/bin:/usr/bin","MY_VAR=hi"],"WorkingDir":"/work"}}}`
	run := func(*exec.Cmd, bool) (string, error) { return payload, nil }

	cfg, err := inspectImageConfig("img:1", run)
	if err != nil {
		t.Fatalf("inspectImageConfig: %v", err)
	}
	if cfg.WorkingDir != "/work" {
		t.Fatalf("WorkingDir = %q, want /work", cfg.WorkingDir)
	}
	if len(cfg.Env) != 2 || cfg.Env[0] != "PATH=/opt/bin:/usr/bin" {
		t.Fatalf("Env = %v", cfg.Env)
	}
}

func TestInspectImageConfigRejectsGarbage(t *testing.T) {
	run := func(*exec.Cmd, bool) (string, error) { return "not json", nil }
	if _, err := inspectImageConfig("img:1", run); err == nil {
		t.Fatal("expected an error for unparseable buildah output")
	}
}

// last wins for a duplicate key in exec's environment, so ordering is what
// decides precedence.
func lastValue(env []string, key string) string {
	prefix := key + "="
	out := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			out = strings.TrimPrefix(kv, prefix)
		}
	}
	return out
}

func TestSessionEnvLetsTheImageOverrideDefaults(t *testing.T) {
	m := &Manager{imageEnv: []string{"PATH=/opt/mytool/bin:/usr/bin", "MY_VAR=hi"}}
	env := m.sessionEnv()
	if got := lastValue(env, "PATH"); got != "/opt/mytool/bin:/usr/bin" {
		t.Fatalf("PATH = %q, want the image's", got)
	}
	if got := lastValue(env, "MY_VAR"); got != "hi" {
		t.Fatalf("MY_VAR = %q", got)
	}
}

func TestSessionEnvKeepsDefaultsAnImageDoesNotSet(t *testing.T) {
	m := &Manager{imageEnv: []string{"MY_VAR=hi"}}
	env := m.sessionEnv()
	if got := lastValue(env, "PATH"); got != "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" {
		t.Fatalf("PATH = %q, want the default", got)
	}
	if got := lastValue(env, "HOME"); got != "/root" {
		t.Fatalf("HOME = %q", got)
	}
}

func TestSessionEnvPlumbingCannotBeShadowedByAnImage(t *testing.T) {
	// bash_init's re-exec reads these; an image must never be able to steer it.
	m := &Manager{imageEnv: []string{
		"WAYPOINT_NAMESPACED=0",
		"WAYPOINT_REEXEC_PATH=/tmp/evil",
	}}
	env := m.sessionEnv()
	if got := lastValue(env, "WAYPOINT_REEXEC_PATH"); got != "/.waypoint/bash_init" {
		t.Fatalf("WAYPOINT_REEXEC_PATH = %q, want waypoint's own", got)
	}
	if got := lastValue(env, "WAYPOINT_NAMESPACED"); got != "1" {
		t.Fatalf("WAYPOINT_NAMESPACED = %q", got)
	}
}

func TestSessionEnvWithoutAnImageIsTheFixedBase(t *testing.T) {
	m := &Manager{}
	env := m.sessionEnv()
	if got := lastValue(env, "PATH"); got != "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" {
		t.Fatalf("PATH = %q", got)
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "LS_COLORS=") || strings.HasPrefix(kv, "SUDO_") {
			t.Fatalf("host environment leaked into the session: %q", kv)
		}
	}
}
