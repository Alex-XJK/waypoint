package waypoint

// CRIU stats-image parsing (stats-dump / stats-restore) and the phase
// breakdown records built from them. The stats images are tiny protobufs;
// they are parsed with a hand-rolled varint reader so pkg/waypoint does not
// grow a protobuf (or crit(1)) dependency. Layout, verified against crit(1)
// on CRIU 4.2:
//
//	u32le IMG_SERVICE magic (0x55105940), u32le STATS magic (0x57093306),
//	u32le payload size, one stats_entry protobuf message. All leaf fields
//	are varints; times are in microseconds.

import (
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	criuImgServiceMagic = 0x55105940
	criuStatsMagic      = 0x57093306
)

// CriuDumpStats is criu's own accounting of a dump, from the stats-dump
// image (field numbers per criu's stats.proto dump_stats_entry).
type CriuDumpStats struct {
	FreezingMs   float64 `json:"freezing_ms"` // seizing/interrupting the tree
	FrozenMs     float64 `json:"frozen_ms"`   // total time the tree was stopped
	MemdumpMs    float64 `json:"memdump_ms"`  // scanning/collecting memory
	MemwriteMs   float64 `json:"memwrite_ms"` // writing page images
	PagesScanned uint64  `json:"pages_scanned"`
	PagesWritten uint64  `json:"pages_written"`
}

// CriuRestoreStats is criu's own accounting of a restore, from the
// stats-restore image (restore_stats_entry).
type CriuRestoreStats struct {
	ForkingMs     float64 `json:"forking_ms"`     // recreating the process tree
	RestoreMs     float64 `json:"restore_ms"`     // criu-internal restore total
	PagesRestored uint64  `json:"pages_restored"` // 4 KiB pages written back
}

// RestoreBreakdown decomposes one fork materialization (checkpoint -> live
// fork). Wall-clock phases are measured by waypoint; Criu* fields come from
// criu's stats-restore image, which lands in the fork's root dir because the
// restore runs with --work-dir there (the shared images dir would be
// clobbered by concurrent forks of the same checkpoint).
type RestoreBreakdown struct {
	TotalMs       float64 `json:"total_ms,omitempty"`        // fork-lock acquire -> running
	HelperMs      float64 `json:"helper_ms"`                 // restore helper: re-exec + namespaces + mount + criu + pidfile
	MountMs       float64 `json:"mount_ms"`                  // overlay + pseudo-fs mounts (inside helper)
	CriuWallMs    float64 `json:"criu_wall_ms"`              // criu restore process wall (inside helper)
	SockWaitMs    float64 `json:"sock_wait_ms"`              // dialing the shell socket until ready
	CriuForkMs    float64 `json:"criu_fork_ms,omitempty"`    // criu: forking_time
	CriuRestoreMs float64 `json:"criu_restore_ms,omitempty"` // criu: restore_time
	PagesRestored uint64  `json:"pages_restored,omitempty"`
}

// String renders the breakdown as flat "key_ms=1.2" tokens for CLI output;
// the bench harness parses these back by suffix.
func (b *RestoreBreakdown) String() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "helper_ms=%.1f mount_ms=%.1f criu_wall_ms=%.1f criu_fork_ms=%.1f criu_restore_ms=%.1f sockwait_ms=%.1f",
		b.HelperMs, b.MountMs, b.CriuWallMs, b.CriuForkMs, b.CriuRestoreMs, b.SockWaitMs)
	if b.PagesRestored > 0 {
		fmt.Fprintf(&sb, " pages_mib=%.1f", float64(b.PagesRestored)/256)
	}
	return sb.String()
}

// SnapshotBreakdown decomposes a snapshot/checkpoint: dump the process tree,
// seal the upper layer, re-restore the fork on top of the new checkpoint.
type SnapshotBreakdown struct {
	TotalMs   float64           `json:"total_ms"`
	DumpMs    float64           `json:"dump_ms"` // criu dump wall
	SealMs    float64           `json:"seal_ms"` // unmount + rename upper + re-mkdir
	RestoreMs float64           `json:"restore_ms"`
	Dump      *CriuDumpStats    `json:"criu_dump,omitempty"`
	Restore   *RestoreBreakdown `json:"restore,omitempty"`
}

func (b *SnapshotBreakdown) String() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "total_ms=%.1f dump_ms=%.1f", b.TotalMs, b.DumpMs)
	if d := b.Dump; d != nil {
		fmt.Fprintf(&sb, " dump_freeze_ms=%.1f dump_frozen_ms=%.1f dump_memdump_ms=%.1f dump_memwrite_ms=%.1f pages_written_mib=%.1f",
			d.FreezingMs, d.FrozenMs, d.MemdumpMs, d.MemwriteMs, float64(d.PagesWritten)/256)
	}
	fmt.Fprintf(&sb, " seal_ms=%.1f restore_ms=%.1f", b.SealMs, b.RestoreMs)
	if b.Restore != nil {
		fmt.Fprintf(&sb, " %s", b.Restore.String())
	}
	return sb.String()
}

func durMs(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

// --- stats image parsing ---

func readCriuDumpStats(path string) (*CriuDumpStats, error) {
	dump, _, err := readCriuStatsEntry(path)
	if err != nil {
		return nil, err
	}
	if dump == nil {
		return nil, fmt.Errorf("%s has no dump entry", path)
	}
	return &CriuDumpStats{
		FreezingMs:   float64(dump[1]) / 1000,
		FrozenMs:     float64(dump[2]) / 1000,
		MemdumpMs:    float64(dump[3]) / 1000,
		MemwriteMs:   float64(dump[4]) / 1000,
		PagesScanned: dump[5],
		PagesWritten: dump[7],
	}, nil
}

func readCriuRestoreStats(path string) (*CriuRestoreStats, error) {
	_, restore, err := readCriuStatsEntry(path)
	if err != nil {
		return nil, err
	}
	if restore == nil {
		return nil, fmt.Errorf("%s has no restore entry", path)
	}
	return &CriuRestoreStats{
		ForkingMs:     float64(restore[3]) / 1000,
		RestoreMs:     float64(restore[4]) / 1000,
		PagesRestored: restore[5],
	}, nil
}

// readCriuStatsEntry returns the dump (field 1) and restore (field 2)
// submessages of a stats image as field-number -> varint-value maps.
func readCriuStatsEntry(path string) (dump, restore map[int]uint64, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	if len(data) < 12 {
		return nil, nil, fmt.Errorf("stats image %s too short (%d bytes)", path, len(data))
	}
	if m := binary.LittleEndian.Uint32(data[0:4]); m != criuImgServiceMagic {
		return nil, nil, fmt.Errorf("stats image %s: bad service magic %#x", path, m)
	}
	if m := binary.LittleEndian.Uint32(data[4:8]); m != criuStatsMagic {
		return nil, nil, fmt.Errorf("stats image %s: bad stats magic %#x", path, m)
	}
	size := int(binary.LittleEndian.Uint32(data[8:12]))
	if size < 0 || 12+size > len(data) {
		return nil, nil, fmt.Errorf("stats image %s: bad payload size %d", path, size)
	}
	msg := data[12 : 12+size]

	for len(msg) > 0 {
		field, wire, n, perr := pbTag(msg)
		if perr != nil {
			return nil, nil, fmt.Errorf("stats image %s: %w", path, perr)
		}
		msg = msg[n:]
		if wire != 2 { // stats_entry has only length-delimited fields
			return nil, nil, fmt.Errorf("stats image %s: unexpected wire type %d for field %d", path, wire, field)
		}
		length, n, perr := pbVarint(msg)
		if perr != nil || int(length) > len(msg[n:]) {
			return nil, nil, fmt.Errorf("stats image %s: truncated field %d", path, field)
		}
		sub := msg[n : n+int(length)]
		msg = msg[n+int(length):]

		fields, perr := pbVarintFields(sub)
		if perr != nil {
			return nil, nil, fmt.Errorf("stats image %s: field %d: %w", path, field, perr)
		}
		switch field {
		case 1:
			dump = fields
		case 2:
			restore = fields
		}
	}
	return dump, restore, nil
}

// pbVarintFields decodes a message whose fields are all varints.
func pbVarintFields(msg []byte) (map[int]uint64, error) {
	out := map[int]uint64{}
	for len(msg) > 0 {
		field, wire, n, err := pbTag(msg)
		if err != nil {
			return nil, err
		}
		msg = msg[n:]
		if wire != 0 {
			return nil, fmt.Errorf("unexpected wire type %d for field %d", wire, field)
		}
		v, n, err := pbVarint(msg)
		if err != nil {
			return nil, err
		}
		out[field] = v
		msg = msg[n:]
	}
	return out, nil
}

func pbTag(b []byte) (field, wire, n int, err error) {
	v, n, err := pbVarint(b)
	if err != nil {
		return 0, 0, 0, err
	}
	return int(v >> 3), int(v & 7), n, nil
}

func pbVarint(b []byte) (uint64, int, error) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		v |= uint64(b[i]&0x7f) << (7 * i)
		if b[i]&0x80 == 0 {
			return v, i + 1, nil
		}
	}
	return 0, 0, fmt.Errorf("truncated varint")
}
