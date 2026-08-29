package waypoint

import "testing"

func TestShellQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/work/f.txt", "'/work/f.txt'"},
		{"with space", "'with space'"},
		{"it's", `'it'\''s'`},
		{"", "''"},
	}
	for _, tc := range cases {
		if got := shellQuote(tc.in); got != tc.want {
			t.Fatalf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
