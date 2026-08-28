package waypoint

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// nameComponent mirrors the canonical Docker/buildah image-reference
// name-component grammar (distribution/reference). A tag of the form
// "waypoint_<component>" must match this or `buildah bud` fails with
// "invalid reference format".
var nameComponent = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*$`)

func TestImageRefComponent(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"trailing underscore", "foo_", "foo"},
		// Regression: mkdtemp(prefix="img3_") dirs end in "_"; also exercise
		// an uppercase letter in the same basename.
		{"uppercase and trailing underscore", "Img3_4L96KK1_", "img3-4l96kk1"},
		{"mkdtemp style lowercase", "img3_4l96kk1_", "img3-4l96kk1"},
		{"leading and trailing separators", "__weird..name--", "weird-name"},
		{"already valid", "python-app", "python-app"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := imageRefComponent(tc.in)
			if got != tc.want {
				t.Fatalf("imageRefComponent(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if tag := "waypoint_" + got; !nameComponent.MatchString(tag) {
				t.Fatalf("derived tag %q is not a valid image reference name", tag)
			}
		})
	}
}

func TestImageRefComponentFallback(t *testing.T) {
	// A basename with no usable characters must still yield a valid,
	// non-empty component via the hash fallback.
	got := imageRefComponent("___")
	if got == "" {
		t.Fatal("expected non-empty fallback component")
	}
	if tag := "waypoint_" + got; !nameComponent.MatchString(tag) {
		t.Fatalf("fallback tag %q is not a valid image reference name", tag)
	}
}

const sampleInspect = `{
  "Type": "buildah 0.0.1",
  "OCIv1": {
    "config": {
      "Env": ["PATH=/opt/mybin:/usr/bin", "GREETING=hello world", "EQ=a=b"],
      "WorkingDir": "/opt",
      "User": "root"
    }
  }
}`

func fakeRun(out string, err error) func(*exec.Cmd, bool) (string, error) {
	return func(*exec.Cmd, bool) (string, error) { return out, err }
}

func TestInspectImageConfigParsesEnvAndWorkingDir(t *testing.T) {
	cfg, err := inspectImageConfig("img", fakeRun(sampleInspect, nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.WorkingDir != "/opt" {
		t.Errorf("WorkingDir = %q, want /opt", cfg.WorkingDir)
	}
	want := []string{"PATH=/opt/mybin:/usr/bin", "GREETING=hello world", "EQ=a=b"}
	if len(cfg.Env) != len(want) {
		t.Fatalf("Env = %v, want %v", cfg.Env, want)
	}
	for i := range want {
		if cfg.Env[i] != want[i] {
			t.Errorf("Env[%d] = %q, want %q", i, cfg.Env[i], want[i])
		}
	}
}

func TestInspectImageConfigEmptyConfig(t *testing.T) {
	cfg, err := inspectImageConfig("img", fakeRun(`{"OCIv1":{"config":{}}}`, nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Env) != 0 || cfg.WorkingDir != "" {
		t.Errorf("got %+v, want zero value", cfg)
	}
}

func TestInspectImageConfigErrors(t *testing.T) {
	if _, err := inspectImageConfig("img", fakeRun("", fmt.Errorf("boom"))); err == nil {
		t.Error("expected the buildah failure to propagate")
	}
	if _, err := inspectImageConfig("img", fakeRun("not json", nil)); err == nil {
		t.Error("expected a parse error for malformed output")
	}
}

func TestHasEnvKey(t *testing.T) {
	env := []string{"PATH=/usr/bin", "PATHOLOGICAL=1"}
	if !hasEnvKey(env, "PATH") {
		t.Error("PATH should be found")
	}
	if hasEnvKey([]string{"PATHOLOGICAL=1"}, "PATH") {
		t.Error("PATHOLOGICAL must not satisfy PATH")
	}
	if hasEnvKey(env, "HOME") {
		t.Error("HOME should be absent")
	}
}

func envValue(env []string, key string) (string, bool) {
	for _, entry := range env {
		if strings.HasPrefix(entry, key+"=") {
			return strings.TrimPrefix(entry, key+"="), true
		}
	}
	return "", false
}

func TestSessionEnvUsesImageConfig(t *testing.T) {
	m := &Manager{
		imageEnv:     []string{"PATH=/opt/mybin", "FOO=bar"},
		imageWorkDir: "/opt",
	}
	env := m.sessionEnv()

	if v, _ := envValue(env, "FOO"); v != "bar" {
		t.Errorf("FOO = %q, want bar", v)
	}
	if v, _ := envValue(env, "PATH"); v != "/opt/mybin" {
		t.Errorf("PATH = %q, want the image value", v)
	}
	if v, _ := envValue(env, "HOME"); v != "/root" {
		t.Errorf("HOME = %q, want the /root fallback", v)
	}
	if _, ok := envValue(env, "WAYPOINT_INIT_WORKDIR"); ok {
		t.Error("the WORKDIR handoff must not leak into the session environment")
	}
}

func TestSessionEnvFallsBackToPath(t *testing.T) {
	m := &Manager{imageEnv: []string{"FOO=bar"}}
	env := m.sessionEnv()
	v, ok := envValue(env, "PATH")
	if !ok {
		t.Fatal("an image without PATH must still get one, or nothing can run")
	}
	if "PATH="+v != FallbackPath {
		t.Errorf("PATH = %q, want the fallback", v)
	}
}

func TestSessionEnvWithoutImageKeepsHostEnvironment(t *testing.T) {
	m := &Manager{}
	env := m.sessionEnv()

	if len(env) == 0 {
		t.Error("expected the host environment to be inherited")
	}
}
