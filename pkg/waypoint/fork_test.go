package waypoint

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func requireBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH; the syntax precheck is skipped on such hosts")
	}
}

func TestCheckCommandSyntaxAcceptsCompleteInput(t *testing.T) {
	requireBash(t)
	for _, command := range []string{
		"true",
		"cd /app && npm test 2>&1 | tail -20",
		`echo "a  b" '$HOME' *.txt; export X=1`,
		"cat <<'EOF'\nline one\nline two\nEOF\n",
		"if true; then echo x; fi",
		"for i in 1 2; do echo $i; done &",
		// A trailing backslash is complete input; bash_init's framing handles it.
		"echo trailing \\",
		"",
	} {
		if err := checkCommandSyntax(command); err != nil {
			t.Fatalf("checkCommandSyntax(%q) = %v, want nil", command, err)
		}
	}
}

// Input that can never complete is refused before the fork lock is taken.
func TestCheckCommandSyntaxRejectsIncompleteInput(t *testing.T) {
	requireBash(t)
	cases := map[string]string{
		`echo "unterminated`:      "unexpected EOF",
		"if true; then echo x":    "syntax error",
		"cat <<EOF\nno delimiter": "delimited by end-of-file",
		"echo ) stray":            "syntax error",
	}
	for command, want := range cases {
		err := checkCommandSyntax(command)
		if !errors.Is(err, ErrCommandSyntax) {
			t.Fatalf("checkCommandSyntax(%q) = %v, want ErrCommandSyntax", command, err)
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("checkCommandSyntax(%q) = %q, want bash's diagnostic containing %q", command, err, want)
		}
		if strings.Contains(err.Error(), "bash: ") {
			t.Fatalf("checkCommandSyntax(%q) = %q, want bash's own name stripped", command, err)
		}
	}
}
