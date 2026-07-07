package waypoint

import (
	"regexp"
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
