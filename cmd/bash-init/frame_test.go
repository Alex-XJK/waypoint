package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runFramed feeds framed bytes to a non-interactive bash and returns the
// command's output and what the completion line wrote to a stand-in FIFO.
func runFramed(t *testing.T, framed string) (stdout, completion string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH")
	}
	fifo := filepath.Join(t.TempDir(), "exec.done")
	framed = strings.ReplaceAll(framed, completionFifoGuestPath, fifo)
	cmd := exec.Command("bash", "--noprofile", "--norc")
	cmd.Stdin = strings.NewReader(framed)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	_ = cmd.Run()
	done, _ := os.ReadFile(fifo)
	return out.String(), string(done)
}

func TestFrameCommandReportsExitStatus(t *testing.T) {
	for command, want := range map[string]string{
		"true":                   "n1 0\n",
		"false":                  "n1 1\n",
		"echo hi; exit_code=3\n": "n1 0\n",
		"(exit 7)":               "n1 7\n",
	} {
		_, completion := runFramed(t, frameCommand(command, "n1", completionFifoGuestPath))
		if completion != want {
			t.Fatalf("frameCommand(%q): completion = %q, want %q", command, completion, want)
		}
	}
}

// A trailing backslash is a line continuation; the blank line frameCommand
// inserts keeps it from swallowing the completion line.
func TestFrameCommandSurvivesTrailingContinuation(t *testing.T) {
	const command = "echo trailing \\"

	stdout, completion := runFramed(t, frameCommand(command, "n2", completionFifoGuestPath))
	if stdout != "trailing\n" {
		t.Fatalf("stdout = %q, want %q", stdout, "trailing\n")
	}
	if completion != "n2 0\n" {
		t.Fatalf("completion = %q, want %q", completion, "n2 0\n")
	}

	// Control: without the separator the completion line becomes echo's
	// arguments and no valid completion reaches the FIFO.
	old := command + "\n" + "builtin printf '%s %s\\n' 'n3' \"$?\" > " + completionFifoGuestPath + "\n"
	_, completion = runFramed(t, old)
	if _, ok := parseCompletion(strings.TrimSpace(completion), "n3"); ok {
		t.Fatalf("control: completion %q parsed as a valid completion; the continuation should have swallowed the line", completion)
	}
	if !strings.Contains(completion, "builtin printf") {
		t.Fatalf("control: FIFO got %q, want the completion line echoed as arguments", completion)
	}
}
