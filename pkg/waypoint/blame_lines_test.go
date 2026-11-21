package waypoint

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// newBlameTestChain makes a session directory with an original rootfs and one ready
// checkpoint per layer ID, each chained on the ones before it. It returns the
// manager, the original rootfs and a function giving a layer's upper directory.
func newBlameTestChain(t *testing.T, layerIDs ...string) (*Manager, string, func(string) string) {
	t.Helper()
	base := t.TempDir()
	m := &Manager{baseDir: base, metadataDir: filepath.Join(base, "metadata")}
	original := filepath.Join(base, "original")
	for _, dir := range []string{original, m.metadataDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	for i, id := range layerIDs {
		if err := os.MkdirAll(m.checkpointUpperDir(id), 0755); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(Metadata{
			ID: id, LayerIDs: layerIDs[:i+1], OriginalDir: original, Status: CheckpointStatusReady,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(m.metadataDir, id+".json"), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	return m, original, func(id string) string { return m.checkpointUpperDir(id) }
}

func blameLinesOf(t *testing.T, m *Manager, ckptID, path string) *BlameLinesResult {
	t.Helper()
	chain, err := m.loadBlameChain(ckptID)
	if err != nil {
		t.Fatal(err)
	}
	res, err := chain.blameLines(m, path)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestBlameLines_CarriesOriginsThroughEdits(t *testing.T) {
	m, original, upper := newBlameTestChain(t, "c1", "c2")
	writeTestFile(t, filepath.Join(original, "app/main.py"), "a\nb\nc\n")
	writeTestFile(t, filepath.Join(upper("c1"), "app/main.py"), "a\nB\nc\nd\n")
	writeTestFile(t, filepath.Join(upper("c2"), "app/main.py"), "x\na\nB\nc\nd\n")

	res := blameLinesOf(t, m, "c2", "app/main.py")

	wantLines := []BlameLineRun{
		{CheckpointID: "c2", Start: 1, Text: []string{"x"}},
		{CheckpointID: "original", Start: 2, Text: []string{"a"}},
		{CheckpointID: "c1", Start: 3, Text: []string{"B"}},
		{CheckpointID: "original", Start: 4, Text: []string{"c"}},
		{CheckpointID: "c1", Start: 5, Text: []string{"d"}},
	}
	if !reflect.DeepEqual(res.Lines, wantLines) {
		t.Errorf("lines = %+v\nwant   %+v", res.Lines, wantLines)
	}
	if want := map[string]int{"original": 2, "c1": 2, "c2": 1}; !reflect.DeepEqual(res.Origins, want) {
		t.Errorf("origins = %v, want %v", res.Origins, want)
	}
	wantVersions := []BlameVersion{{"c1", ChangeTypeModified}, {"c2", ChangeTypeModified}}
	if !reflect.DeepEqual(res.Versions, wantVersions) {
		t.Errorf("versions = %+v, want %+v", res.Versions, wantVersions)
	}
	if !res.Exists || res.FileType != FileTypeRegular || res.Binary || res.Approximate || res.LineCount != 5 {
		t.Errorf("unexpected flags: %+v", res)
	}
	if res.TargetPath != "/app/main.py" || res.TargetCkptID != "c2" {
		t.Errorf("target = %s at %s", res.TargetPath, res.TargetCkptID)
	}
}

func TestBlameLines_TargetIsAnEarlierCheckpoint(t *testing.T) {
	m, _, upper := newBlameTestChain(t, "c1", "c2")
	writeTestFile(t, filepath.Join(upper("c1"), "f"), "one\n")
	writeTestFile(t, filepath.Join(upper("c2"), "f"), "one\ntwo\n")

	res := blameLinesOf(t, m, "c1", "/f")
	if want := map[string]int{"c1": 1}; !reflect.DeepEqual(res.Origins, want) || len(res.Versions) != 1 {
		t.Errorf("blame at c1 saw a later layer: %+v", res)
	}
}

func TestBlameLines_RecreatedFileStartsOver(t *testing.T) {
	m, _, upper := newBlameTestChain(t, "c1", "c2", "c3")
	writeTestFile(t, filepath.Join(upper("c1"), "f"), "kept\n")
	writeTestCharacterDeviceWhiteout(t, filepath.Join(upper("c2"), "f"))
	writeTestFile(t, filepath.Join(upper("c3"), "f"), "kept\n")

	res := blameLinesOf(t, m, "c3", "/f")
	if want := map[string]int{"c3": 1}; !reflect.DeepEqual(res.Origins, want) {
		t.Errorf("origins = %v, want %v (a deletion ends the history)", res.Origins, want)
	}
	wantVersions := []BlameVersion{{"c1", ChangeTypeAdded}, {"c2", ChangeTypeDeleted}, {"c3", ChangeTypeAdded}}
	if !reflect.DeepEqual(res.Versions, wantVersions) {
		t.Errorf("versions = %+v, want %+v", res.Versions, wantVersions)
	}

	gone := blameLinesOf(t, m, "c2", "/f")
	if gone.Exists || gone.LineCount != 0 || len(gone.Lines) != 0 {
		t.Errorf("deleted file reported lines: %+v", gone)
	}
}

func TestBlameLines_BinaryAndSymlinkHaveNoLines(t *testing.T) {
	m, _, upper := newBlameTestChain(t, "c1", "c2")
	writeTestFile(t, filepath.Join(upper("c1"), "model.bin"), "head\x00tail\n")
	writeTestFile(t, filepath.Join(upper("c1"), "target"), "t\n")
	if err := os.Symlink("target", filepath.Join(upper("c1"), "link")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(upper("c2"), "model.bin"), "now text\n")

	bin := blameLinesOf(t, m, "c1", "/model.bin")
	if !bin.Exists || !bin.Binary || bin.LineCount != 0 || len(bin.Lines) != 0 {
		t.Errorf("binary version: %+v", bin)
	}
	text := blameLinesOf(t, m, "c2", "/model.bin")
	if text.Binary || !reflect.DeepEqual(text.Origins, map[string]int{"c2": 1}) {
		t.Errorf("text after binary: %+v", text)
	}
	link := blameLinesOf(t, m, "c1", "/link")
	if !link.Exists || link.FileType != FileTypeSymlink || link.LineCount != 0 {
		t.Errorf("symlink: %+v", link)
	}
}

func TestBlameLines_AbsentPath(t *testing.T) {
	m, _, _ := newBlameTestChain(t, "c1")
	res := blameLinesOf(t, m, "c1", "/nowhere")
	if res.Exists || len(res.Versions) != 0 || len(res.Lines) != 0 || len(res.Origins) != 0 {
		t.Errorf("absent path: %+v", res)
	}
}

func TestPrintBlameMany(t *testing.T) {
	m, original, upper := newBlameTestChain(t, "c1")
	writeTestFile(t, filepath.Join(original, "a"), "1\n")
	writeTestFile(t, filepath.Join(upper("c1"), "a"), "1\n2\n")
	writeTestFile(t, filepath.Join(upper("c1"), "b"), "new\n")

	var out bytes.Buffer
	if err := m.PrintBlameMany("c1", []string{"/a", "b", "/"}, true, &out); err != nil {
		t.Fatal(err)
	}
	var items []map[string]any
	if err := json.Unmarshal(out.Bytes(), &items); err != nil {
		t.Fatalf("output is not a JSON array: %v\n%s", err, out.String())
	}
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3", len(items))
	}
	if items[0]["target_path"] != "/a" || items[0]["line_count"] != 2.0 {
		t.Errorf("first item: %v", items[0])
	}
	if items[1]["target_path"] != "/b" || items[1]["origins"].(map[string]any)["c1"] != 1.0 {
		t.Errorf("second item: %v", items[1])
	}
	if items[2]["target_path"] != "/" || items[2]["error"] == nil {
		t.Errorf("root path should be an error entry: %v", items[2])
	}

	out.Reset()
	if err := m.PrintBlameMany("c1", []string{"/a"}, false, &out); err != nil {
		t.Fatal(err)
	}
	var hunks []BlameResult
	if err := json.Unmarshal(out.Bytes(), &hunks); err != nil {
		t.Fatal(err)
	}
	if len(hunks) != 1 || len(hunks[0].Records) != 1 || hunks[0].Records[0].ChangeType != ChangeTypeModified {
		t.Errorf("hunk blame: %+v", hunks)
	}

	out.Reset()
	if err := m.PrintBlameMany("c1", nil, true, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "[]\n" {
		t.Errorf("empty batch printed %q", out.String())
	}

	if err := m.PrintBlameMany("missing", []string{"/a"}, true, &out); err == nil {
		t.Errorf("unknown checkpoint should fail the call")
	}
}

func TestBlameLines_FinalNewlineIsAChange(t *testing.T) {
	m, original, upper := newBlameTestChain(t, "c1")
	writeTestFile(t, filepath.Join(original, "f"), "a\nb")
	writeTestFile(t, filepath.Join(upper("c1"), "f"), "a\nb\n")

	res := blameLinesOf(t, m, "c1", "/f")
	want := []BlameLineRun{
		{CheckpointID: "original", Start: 1, Text: []string{"a"}},
		{CheckpointID: "c1", Start: 2, Text: []string{"b"}},
	}
	if !reflect.DeepEqual(res.Lines, want) {
		t.Errorf("lines = %+v, want %+v (git also dates a line whose newline was added)", res.Lines, want)
	}
}
