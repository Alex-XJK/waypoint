package waypoint

import (
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

// splitEnvKey returns the variable name of a "KEY=VALUE" entry.
func splitEnvKey(entry string) string {
	if i := strings.IndexByte(entry, '='); i > 0 {
		return entry[:i]
	}
	return ""
}

func TestSessionEnvIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, entry := range (&Manager{}).sessionEnv() {
		key := splitEnvKey(entry)
		if key == "" {
			t.Fatalf("environment entry %q is not KEY=VALUE", entry)
		}
		if seen[key] {
			// A duplicate would make the guest's value depend on os/exec's
			// dedup rules rather than on this list.
			t.Fatalf("environment defines %s twice", key)
		}
		seen[key] = true
	}
	for _, key := range []string{"PATH", "HOME", "TERM", "LANG", "WAYPOINT_NAMESPACED", "WAYPOINT_REEXEC_PATH"} {
		if !seen[key] {
			t.Fatalf("session environment is missing %s", key)
		}
	}
}

// TestUnattendedEnvReachesTheGuest guards a cross-package contract: bash_init
// drops every WAYPOINT_* and TERM= entry before handing the environment to
// the shell (see cmd/bash-init/main.go). An unattended variable named to
// collide with that filter would be silently discarded.
func TestUnattendedEnvReachesTheGuest(t *testing.T) {
	for _, entry := range unattendedGuestEnv {
		if strings.HasPrefix(entry, "WAYPOINT_") || strings.HasPrefix(entry, "TERM=") {
			t.Fatalf("%q is stripped by bash_init and would never reach the shell", entry)
		}
	}
}

// TestUnattendedEnvCoversInteractiveTooling pins the tools a fork's shell must
// not stall on: it is driven over a socket, so a prompt is a hang, not a
// cosmetic issue.
func TestUnattendedEnvCoversInteractiveTooling(t *testing.T) {
	present := map[string]bool{}
	for _, entry := range unattendedGuestEnv {
		present[splitEnvKey(entry)] = true
	}
	for _, key := range []string{
		"PAGER", "GIT_PAGER", "SYSTEMD_PAGER",
		"EDITOR", "VISUAL", "GIT_EDITOR", "GIT_SEQUENCE_EDITOR",
		"GIT_TERMINAL_PROMPT", "GIT_ASKPASS", "GIT_SSH_COMMAND",
		"DEBIAN_FRONTEND", "NEEDRESTART_MODE",
		"OPAMYES", "OPAMCONFIRMLEVEL",
		"PYTHONUNBUFFERED", "PIP_NO_INPUT", "PIP_DISABLE_PIP_VERSION_CHECK", "PIP_PROGRESS_BAR",
	} {
		if !present[key] {
			t.Errorf("unattended environment lost %s", key)
		}
	}
}
