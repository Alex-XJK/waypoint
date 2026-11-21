package waypoint

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// recordsOfVersions blames a file whose successive versions have the given
// contents.
func recordsOfVersions(t *testing.T, contents ...string) []BlameCkptRecord {
	t.Helper()
	records, err := buildBlameRecords(eventsOfVersions(t, contents))
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func TestBlameBinaryVersionsHaveNoLines(t *testing.T) {
	const text, otherText = "one\ntwo\n", "one\n2\n"
	const binary, otherBinary = "\x7fELF\x00\x01\x02\n", "\x7fELF\x00\x03\x04\n"
	cases := []struct {
		name       string
		prev, next string
		change     ChangeType
		binary     bool
		lines      bool
	}{
		{"text to text", text, otherText, ChangeTypeModified, false, true},
		{"text to binary", text, binary, ChangeTypeModified, true, false},
		{"binary to text", binary, text, ChangeTypeModified, true, false},
		{"binary to other binary", binary, otherBinary, ChangeTypeModified, true, false},
		{"same binary content", binary, binary, ChangeTypeMetadata, false, false},
		{"same text content", text, text, ChangeTypeMetadata, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := recordsOfVersions(t, tc.prev, tc.next)[1]
			if record.ChangeType != tc.change || record.Binary != tc.binary || (len(record.LineByLine) > 0) != tc.lines {
				t.Fatalf("record = %s binary=%v with %d lines, want %s binary=%v lines=%v",
					record.ChangeType, record.Binary, len(record.LineByLine), tc.change, tc.binary, tc.lines)
			}
		})
	}
}

func TestBlameAddedAndDeletedBinaryFile(t *testing.T) {
	events := eventsOfVersions(t, []string{"\x00\x01\x02"})
	added := events[0]
	deleted := &blameEvent{prev: added.next, next: state{ckptID: "c1"}, change: ChangeTypeDeleted}
	records, err := buildBlameRecords([]*blameEvent{added, deleted})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []ChangeType{ChangeTypeAdded, ChangeTypeDeleted} {
		if r := records[i]; r.ChangeType != want || !r.Binary || len(r.LineByLine) != 0 {
			t.Fatalf("record %d = %s binary=%v with %d lines, want %s, binary, no lines", i, r.ChangeType, r.Binary, len(r.LineByLine), want)
		}
	}

	// The same for a text file keeps its lines.
	records = recordsOfVersions(t, "one\n")
	if r := records[0]; r.ChangeType != ChangeTypeAdded || r.Binary || len(r.LineByLine) == 0 {
		t.Fatalf("added text file: %s binary=%v with %d lines", r.ChangeType, r.Binary, len(r.LineByLine))
	}
}

// As in git, only the first 8000 bytes are looked at.
func TestIsBinaryFileLooksAtTheFirst8000Bytes(t *testing.T) {
	withNULAt := func(index, size int) string {
		content := []byte(strings.Repeat("x", size))
		content[index] = 0
		return string(content)
	}
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{"empty", "", false},
		{"short text", "hello\n", false},
		{"short with a NUL", "a\x00b", true},
		{"NUL in the last byte looked at", withNULAt(blameBinarySniffBytes-1, 9000), true},
		{"NUL just after it", withNULAt(blameBinarySniffBytes, 9000), false},
		{"long text", strings.Repeat("line\n", 4000), false},
	}
	bufs := newCompareBufs()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "f")
			writeTestFile(t, path, tc.content)
			got, err := isBinaryFile(path, int64(len(tc.content)), bufs.left)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("isBinaryFile = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBlameTreatsVeryLargeFilesAsBinary(t *testing.T) {
	saved := blameBigFileBytes
	blameBigFileBytes = 64
	defer func() { blameBigFileBytes = saved }()

	small, large := "short\n", strings.Repeat("a long line of text\n", 10)
	records := recordsOfVersions(t, small, small+"more\n", large, large+"more\n")
	want := []bool{false, false, true, true}
	for i, record := range records {
		if record.Binary != want[i] || (len(record.LineByLine) == 0) != want[i] {
			t.Fatalf("record %d: binary=%v with %d lines, want binary=%v", i, record.Binary, len(record.LineByLine), want[i])
		}
	}
}

func TestBlameRecordJSONMarksBinaryOnly(t *testing.T) {
	records := recordsOfVersions(t, "one\n", "\x00\x01")
	text, err := json.Marshal(records[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(text), "binary") {
		t.Fatalf("text record mentions binary: %s", text)
	}
	binary, err := json.Marshal(records[1])
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"checkpoint_id":"c1","change_type":"modified","line_by_line_diff":[],"binary":true}`; string(binary) != want {
		t.Fatalf("binary record = %s\nwant %s", binary, want)
	}
}
