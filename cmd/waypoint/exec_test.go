package main

import (
	"strings"
	"testing"
)

func TestParseExecCommandPassesOneArgumentThrough(t *testing.T) {
	// The fork's shell parses the string; nothing here may touch it.
	for _, want := range []string{
		"ls",
		"cd /app && npm test 2>&1 | tail -20",
		`echo "a  b" '$HOME' *.txt`,
		"cat <<'EOF'\nline one\nline two\nEOF\n",
		"echo trailing \\",
	} {
		got, err := parseExecCommand([]string{want})
		if err != nil {
			t.Fatalf("parseExecCommand(%q) error = %v", want, err)
		}
		if got != want {
			t.Fatalf("parseExecCommand(%q) = %q, want it unchanged", want, got)
		}
	}
}

func TestParseExecCommandRejectsMissingCommand(t *testing.T) {
	if _, err := parseExecCommand(nil); err == nil {
		t.Fatal("parseExecCommand(nil) = nil error, want usage error")
	}
}

// Several arguments used to be joined with spaces and re-parsed by the fork's
// shell, so `-- cat "/root/a b"` ran `cat /root/a b`. They are refused, and
// the hint shows the caller the single-string spelling of what they typed.
func TestParseExecCommandRejectsMultipleArguments(t *testing.T) {
	_, err := parseExecCommand([]string{"cat", "/root/a b", "it's"})
	if err == nil {
		t.Fatal("parseExecCommand(3 args) = nil error, want rejection")
	}
	msg := err.Error()
	for _, want := range []string{"got 3 arguments", `-- 'cat /root/a b it'\''s'`} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q does not contain %q", msg, want)
		}
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"plain":         "'plain'",
		"a b":           "'a b'",
		"it's":          `'it'\''s'`,
		"$HOME `id` \\": "'$HOME `id` \\'",
	} {
		if got := shellQuote(in); got != want {
			t.Fatalf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}
