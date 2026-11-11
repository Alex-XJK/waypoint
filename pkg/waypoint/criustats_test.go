package waypoint

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeStatsDumpImage writes a stats image holding one dump_stats_entry
// with the given varint fields, laid out as criu writes it.
func writeStatsDumpImage(t *testing.T, fields map[int]uint64) string {
	t.Helper()
	var entry []byte
	for field := 1; field <= 16; field++ {
		if v, ok := fields[field]; ok {
			entry = binary.AppendUvarint(entry, uint64(field<<3)) // wire type 0
			entry = binary.AppendUvarint(entry, v)
		}
	}
	msg := binary.AppendUvarint(nil, 1<<3|2) // stats_entry.dump, length-delimited
	msg = binary.AppendUvarint(msg, uint64(len(entry)))
	msg = append(msg, entry...)

	data := binary.LittleEndian.AppendUint32(nil, criuImgServiceMagic)
	data = binary.LittleEndian.AppendUint32(data, criuStatsMagic)
	data = binary.LittleEndian.AppendUint32(data, uint32(len(msg)))
	data = append(data, msg...)

	path := filepath.Join(t.TempDir(), "stats-dump")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile(stats-dump): %v", err)
	}
	return path
}

func TestReadCriuDumpStatsIncremental(t *testing.T) {
	path := writeStatsDumpImage(t, map[int]uint64{
		1: 164, 2: 32133, 3: 7527, 4: 6525,
		5: 2884, 6: 2560, 7: 256,
	})
	ds, err := readCriuDumpStats(path)
	if err != nil {
		t.Fatalf("readCriuDumpStats: %v", err)
	}
	want := CriuDumpStats{
		FreezingMs: 0.164, FrozenMs: 32.133, MemdumpMs: 7.527, MemwriteMs: 6.525,
		PagesScanned: 2884, PagesSkippedParent: 2560, PagesWritten: 256,
	}
	if *ds != want {
		t.Fatalf("readCriuDumpStats = %+v, want %+v", *ds, want)
	}

	out := (&SnapshotBreakdown{Dump: ds}).String()
	for _, token := range []string{"pages_written_mib=1.0", "pages_skipped_parent_mib=10.0"} {
		if !strings.Contains(out, token) {
			t.Errorf("SnapshotBreakdown.String() = %q, missing %q", out, token)
		}
	}
}

func TestSnapshotBreakdownOmitsSkippedPagesForFullDumps(t *testing.T) {
	ds, err := readCriuDumpStats(writeStatsDumpImage(t, map[int]uint64{5: 2884, 6: 0, 7: 2809}))
	if err != nil {
		t.Fatalf("readCriuDumpStats: %v", err)
	}
	if out := (&SnapshotBreakdown{Dump: ds}).String(); strings.Contains(out, "pages_skipped_parent") {
		t.Fatalf("SnapshotBreakdown.String() = %q, want no pages_skipped_parent for a full dump", out)
	}
}
