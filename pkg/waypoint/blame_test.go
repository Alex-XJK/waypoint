package waypoint

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write test file: %v", err)
	}

	return path
}

func TestConstructLineByLine(t *testing.T) {
	tests := []struct {
		name       string
		changeType ChangeType
		oldContent string
		newContent string
		oldCkptID  string
		newCkptID  string
		want       string
	}{
		{
			name:       "added file",
			changeType: ChangeTypeAdded,
			newContent: "hello\nworld\n",
			oldCkptID:  "B",
			newCkptID:  "F",
			want:       "--- B\n+++ F\n@@ -0,0 +1,2 @@\n+hello\n+world\n",
		},
		{
			name:       "deleted file",
			changeType: ChangeTypeDeleted,
			oldContent: "hello\nworld\n",
			oldCkptID:  "B",
			newCkptID:  "F",
			want:       "--- B\n+++ F\n@@ -1,2 +0,0 @@\n-hello\n-world\n",
		},
		{
			name:       "modified file",
			changeType: ChangeTypeModified,
			oldContent: "hello\nworld\n",
			newContent: "hello\ngopher\n",
			oldCkptID:  "B",
			newCkptID:  "F",
			want:       "--- B\n+++ F\n@@ -1,2 +1,2 @@\n hello\n-world\n+gopher\n",
		},
		{
			name:       "inserted line",
			changeType: ChangeTypeModified,
			oldContent: "one\nthree\n",
			newContent: "one\ntwo\nthree\n",
			oldCkptID:  "B",
			newCkptID:  "F",
			want:       "--- B\n+++ F\n@@ -1,2 +1,3 @@\n one\n+two\n three\n",
		},
		{
			name:       "deleted line",
			changeType: ChangeTypeModified,
			oldContent: "one\ntwo\nthree\n",
			newContent: "one\nthree\n",
			oldCkptID:  "B",
			newCkptID:  "F",
			want:       "--- B\n+++ F\n@@ -1,3 +1,2 @@\n one\n-two\n three\n",
		},
		{
			name:       "identical files",
			changeType: ChangeTypeModified,
			oldContent: "hello\nworld\n",
			newContent: "hello\nworld\n",
			oldCkptID:  "B",
			newCkptID:  "F",
			want:       "",
		},
		{
			name:       "empty added file",
			changeType: ChangeTypeAdded,
			oldCkptID:  "B",
			newCkptID:  "F",
			want:       "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()

			prevState := &state{
				ckptID: tt.oldCkptID,
			}
			newState := &state{
				ckptID: tt.newCkptID,
			}

			if tt.changeType != ChangeTypeAdded {
				prevState.visible = true
				prevState.rootViewPath = writeTestFile(
					t, dir, "old.txt", tt.oldContent,
				)
			}

			if tt.changeType != ChangeTypeDeleted {
				newState.visible = true
				newState.rootViewPath = writeTestFile(
					t, dir, "new.txt", tt.newContent,
				)
			}

			got, err := constructLineByLine(
				prevState,
				newState,
				tt.changeType,
			)
			if err != nil {
				t.Fatalf("constructLineByLine() error = %v", err)
			}

			if got != tt.want {
				t.Errorf(
					"constructLineByLine() mismatch\n got:\n%s\nwant:\n%s",
					got,
					tt.want,
				)
			}
		})
	}
}

func TestConstructLineByLineNonRegularFiles(t *testing.T) {
	dir := t.TempDir()

	oldPath := writeTestFile(t, dir, "old.txt", "old content\n")
	newPath := filepath.Join(dir, "new")

	if err := os.Mkdir(newPath, 0755); err != nil {
		t.Fatalf("create test directory: %v", err)
	}

	prevState := &state{
		visible:      true,
		rootViewPath: oldPath,
		ckptID:       "B",
	}
	newState := &state{
		visible:      true,
		rootViewPath: newPath,
		ckptID:       "F",
	}

	got, err := constructLineByLine(
		prevState,
		newState,
		ChangeTypeModified,
	)
	if err != nil {
		t.Fatalf("constructLineByLine() error = %v", err)
	}

	if got != "" {
		t.Errorf("expected empty diff for non-regular file, got:\n%s", got)
	}
}

func TestConstructLineByLineMissingFile(t *testing.T) {
	dir := t.TempDir()

	prevState := &state{
		visible:      true,
		rootViewPath: filepath.Join(dir, "missing.txt"),
		ckptID:       "B",
	}
	newState := &state{
		visible:      true,
		rootViewPath: writeTestFile(t, dir, "new.txt", "hello\n"),
		ckptID:       "F",
	}

	_, err := constructLineByLine(
		prevState,
		newState,
		ChangeTypeModified,
	)
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}
