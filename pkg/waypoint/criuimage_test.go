package waypoint

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// pbw builds protobuf messages for tests.
type pbw struct{ b []byte }

func (w *pbw) varint(v uint64) {
	for v >= 0x80 {
		w.b = append(w.b, byte(v)|0x80)
		v >>= 7
	}
	w.b = append(w.b, byte(v))
}

func (w *pbw) u(field int, v uint64) *pbw {
	w.varint(uint64(field)<<3 | 0)
	w.varint(v)
	return w
}

func (w *pbw) bytes(field int, b []byte) *pbw {
	w.varint(uint64(field)<<3 | 2)
	w.varint(uint64(len(b)))
	w.b = append(w.b, b...)
	return w
}

func (w *pbw) packed(field int, vs ...uint64) *pbw {
	var inner pbw
	for _, v := range vs {
		inner.varint(v)
	}
	return w.bytes(field, inner.b)
}

func writeImage(t *testing.T, path string, magic uint32, msgs ...[]byte) {
	t.Helper()
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, uint32(criuImgCommonMagic))
	binary.Write(&buf, binary.LittleEndian, magic)
	for _, m := range msgs {
		binary.Write(&buf, binary.LittleEndian, uint32(len(m)))
		buf.Write(m)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func filledPage(c byte) []byte { return bytes.Repeat([]byte{c}, criuPageSize) }

func TestParsePBSkipsUnknownAndReadsPacked(t *testing.T) {
	var w pbw
	w.u(1, 7).bytes(99, []byte("unknown")).packed(5, 1, 300, 2).u(5, 9).bytes(6, []byte("comm"))
	m, err := parsePB(w.b)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := m.uint(1); v != 7 {
		t.Fatalf("field 1 = %d", v)
	}
	if got, _ := m.uints(5); len(got) != 4 || got[1] != 300 || got[3] != 9 {
		t.Fatalf("repeated field 5 = %v", got)
	}
	if m.str(6) != "comm" {
		t.Fatalf("field 6 = %q", m.str(6))
	}
}

func TestReadCriuImageChecksMagic(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.img")
	writeImage(t, p, criuFsMagic, (&pbw{}).u(1, 3).b)
	if _, err := readCriuImage(p, criuMmMagic); err == nil {
		t.Fatal("expected a magic mismatch")
	}
	m, err := readCriuImageOne(p, criuFsMagic)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := m.uint(1); v != 3 {
		t.Fatalf("cwd_id = %d", v)
	}
}

// TestPageSource covers raw, parent, zero-block, raw-block, compressed-block
// and payload-aligned entries.
func TestPageSource(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "parent-set")
	child := filepath.Join(t.TempDir(), "child-set")
	for _, d := range []string{parent, child} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	const pid = 42
	// Parent: one raw page at 0x10000.
	writeImage(t, filepath.Join(parent, "pagemap-42.img"), criuPagemapMagic,
		(&pbw{}).u(1, 1).b,
		(&pbw{}).u(1, 0x10000).u(2, 0).u(4, pePRESENT).u(5, 1).b)
	os.WriteFile(filepath.Join(parent, "pages-1.img"), filledPage('P'), 0o644)

	blocks := (&pbw{}).packed(1, 0, 2*criuPageSize, 123).u(2, 2*criuPageSize+123).u(3, 2).b
	writeImage(t, filepath.Join(child, "pagemap-42.img"), criuPagemapMagic,
		(&pbw{}).u(1, 7).b,
		// 0x10000 left in the parent
		(&pbw{}).u(1, 0x10000).u(2, 0).u(4, pePARENT).u(5, 1).b,
		// 0x20000: one raw page
		(&pbw{}).u(1, 0x20000).u(2, 0).u(4, pePRESENT).u(5, 1).b,
		// 0x30000: 5 pages in blocks of 2: a zero block, a raw block, a
		// compressed block (its last, 1-page block is 123 bytes)
		(&pbw{}).u(1, 0x30000).u(2, 0).u(4, pePRESENT).u(5, 5).bytes(6, blocks).b,
		// 0x40000: aligned raw page
		(&pbw{}).u(1, 0x40000).u(2, 0).u(4, pePRESENT|pePAYLOADALIGNED).u(5, 1).b)
	var pages bytes.Buffer
	pages.Write(filledPage('R'))                // 0x20000
	pages.Write(filledPage('a'))                // block 1, page 0x32000
	pages.Write(filledPage('b'))                // block 1, page 0x33000
	pages.Write(bytes.Repeat([]byte{'z'}, 123)) // block 2, compressed
	pages.Write(make([]byte, criuPageSize-123)) // padding to the page boundary
	pages.Write(filledPage('A'))                // aligned 0x40000
	os.WriteFile(filepath.Join(child, "pages-7.img"), pages.Bytes(), 0o644)
	if err := os.Symlink(parent, filepath.Join(child, "parent")); err != nil {
		t.Fatal(err)
	}

	s, err := openPageSource(child, pid)
	if err != nil {
		t.Fatal(err)
	}
	want := map[uint64]byte{0x10000: 'P', 0x20000: 'R', 0x30000: 0, 0x31000: 0, 0x32000: 'a', 0x33000: 'b', 0x40000: 'A'}
	for va, c := range want {
		pg, err := s.page(va)
		if err != nil {
			t.Fatalf("page %#x: %v", va, err)
		}
		if !bytes.Equal(pg, filledPage(c)) {
			t.Fatalf("page %#x starts with %q, want %q", va, pg[0], c)
		}
	}
	if _, err := s.page(0x34000); !errors.Is(err, errPageCompressed) {
		t.Fatalf("page in compressed block: err = %v", err)
	}
	if _, err := s.page(0x90000); !errors.Is(err, errPageMissing) {
		t.Fatalf("page outside the pagemap: err = %v", err)
	}
	got, err := s.readRange(0x32ffe, 0x33002)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "aabb" {
		t.Fatalf("range across two pages of a block = %q", got)
	}
	if got, err := s.readRange(0x10000, 0x10004); err != nil || string(got) != "PPPP" {
		t.Fatalf("range in a parent page = %q, %v", got, err)
	}
	if _, err := s.readRange(0x10ffe, 0x11002); !errors.Is(err, errPageMissing) {
		t.Fatalf("range into an unmapped page: err = %v", err)
	}
}
