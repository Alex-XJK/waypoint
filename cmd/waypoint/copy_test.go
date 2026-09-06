package main

import "testing"

func TestParseCopyEndpoint(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		want      copyEndpoint
		wantError bool
	}{
		{
			name:  "absolute host path",
			value: "/tmp/results:latest.json",
			want:  copyEndpoint{hostPath: "/tmp/results:latest.json"},
		},
		{
			name:  "relative host path containing directory",
			value: "results/output:latest.json",
			want:  copyEndpoint{hostPath: "results/output:latest.json"},
		},
		{
			name:  "fork absolute path",
			value: "branch-a:/workspace/result.json",
			want:  copyEndpoint{forkID: "branch-a", guestPath: "/workspace/result.json"},
		},
		{
			name:  "fork path retains colon",
			value: "branch-a:/workspace/result:latest.json",
			want:  copyEndpoint{forkID: "branch-a", guestPath: "/workspace/result:latest.json"},
		},
		{
			name:  "bare host path containing colon",
			value: "image:latest",
			want:  copyEndpoint{hostPath: "image:latest"},
		},
		{
			name:      "empty fork path",
			value:     "branch-a:",
			wantError: true,
		},
		{
			name:      "empty path",
			value:     "",
			wantError: true,
		},
		{
			name:      "invalid fork ID",
			value:     "-branch:/workspace/result.json",
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCopyEndpoint(tt.value)
			if tt.wantError {
				if err == nil {
					t.Fatalf("parseCopyEndpoint(%q) succeeded, want error", tt.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCopyEndpoint(%q): %v", tt.value, err)
			}
			if got != tt.want {
				t.Fatalf("parseCopyEndpoint(%q) = %+v, want %+v", tt.value, got, tt.want)
			}
		})
	}
}

func TestRunCopyRejectsInvalidEndpointCombinations(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "wrong argument count", args: []string{"session", "source"}},
		{name: "two host paths", args: []string{"session", "source", "destination"}},
		{name: "two fork paths", args: []string{"session", "a:/source", "b:/destination"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := runCopy(tt.args); err == nil {
				t.Fatalf("runCopy(%q) succeeded, want error", tt.args)
			}
		})
	}
}
