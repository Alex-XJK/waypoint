package waypoint

// Read-only access to the CRIU images of a checkpoint, for `state` and `diff`.
//
// The images are protobuf messages framed by CRIU. They are parsed here with
// a small wire-format reader instead of a protobuf dependency or crit(1), in
// the same way as the stats images (criustats.go). Unknown fields are
// skipped, so fields that a newer CRIU adds do not break the reader. Field
// numbers follow the .proto files of Waypoint's CRIU build (images/*.proto).
//
// Image layout: u32le IMG_COMMON magic, u32le image magic, then entries, each
// a u32le payload size followed by one message. pages-*.img is raw: page
// payloads with no header, located through the pagemap entries.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

const (
	criuImgCommonMagic = 0x54564319

	criuPstreeMagic  = 0x50273030
	criuCoreMagic    = 0x55053847
	criuIdsMagic     = 0x54432030
	criuFsMagic      = 0x51403912
	criuMmMagic      = 0x57492820
	criuFilesMagic   = 0x56303138
	criuFdinfoMagic  = 0x56213732
	criuPagemapMagic = 0x56084025

	criuPageSize = 4096
)

// pbField is one decoded protobuf field: a varint (wire 0), a fixed value
// (wire 1/5, in v) or length-delimited bytes (wire 2, in b).
type pbField struct {
	num  int
	wire int
	v    uint64
	b    []byte
}

type pbMsg []pbField

func parsePB(b []byte) (pbMsg, error) {
	var out pbMsg
	for len(b) > 0 {
		num, wire, n, err := pbTag(b)
		if err != nil {
			return nil, err
		}
		b = b[n:]
		f := pbField{num: num, wire: wire}
		switch wire {
		case 0:
			v, n, err := pbVarint(b)
			if err != nil {
				return nil, fmt.Errorf("field %d: %w", num, err)
			}
			f.v, b = v, b[n:]
		case 1:
			if len(b) < 8 {
				return nil, fmt.Errorf("field %d: truncated fixed64", num)
			}
			f.v, b = binary.LittleEndian.Uint64(b), b[8:]
		case 2:
			l, n, err := pbVarint(b)
			if err != nil || l > uint64(len(b)-n) {
				return nil, fmt.Errorf("field %d: truncated bytes", num)
			}
			f.b, b = b[n:n+int(l)], b[n+int(l):]
		case 5:
			if len(b) < 4 {
				return nil, fmt.Errorf("field %d: truncated fixed32", num)
			}
			f.v, b = uint64(binary.LittleEndian.Uint32(b)), b[4:]
		default:
			return nil, fmt.Errorf("field %d: unsupported wire type %d", num, wire)
		}
		out = append(out, f)
	}
	return out, nil
}

// uint returns the last scalar value of field num (protobuf's rule for a
// repeated occurrence of a singular field).
func (m pbMsg) uint(num int) (uint64, bool) {
	for i := len(m) - 1; i >= 0; i-- {
		if m[i].num == num && m[i].wire != 2 {
			return m[i].v, true
		}
	}
	return 0, false
}

func (m pbMsg) bytes(num int) ([]byte, bool) {
	for i := len(m) - 1; i >= 0; i-- {
		if m[i].num == num && m[i].wire == 2 {
			return m[i].b, true
		}
	}
	return nil, false
}

func (m pbMsg) str(num int) string {
	b, _ := m.bytes(num)
	return string(b)
}

// msg parses the embedded message in field num; ok is false when absent.
func (m pbMsg) msg(num int) (pbMsg, bool, error) {
	b, ok := m.bytes(num)
	if !ok {
		return nil, false, nil
	}
	sub, err := parsePB(b)
	return sub, err == nil, err
}

// uints returns every value of a repeated integer field, packed or not.
func (m pbMsg) uints(num int) ([]uint64, error) {
	var out []uint64
	for _, f := range m {
		if f.num != num {
			continue
		}
		if f.wire != 2 {
			out = append(out, f.v)
			continue
		}
		b := f.b
		for len(b) > 0 {
			v, n, err := pbVarint(b)
			if err != nil {
				return nil, fmt.Errorf("packed field %d: %w", num, err)
			}
			out = append(out, v)
			b = b[n:]
		}
	}
	return out, nil
}

// readCriuImage returns the entries of a protobuf image, checking its magic.
// A missing image is returned as an error satisfying os.IsNotExist.
func readCriuImage(path string, magic uint32) ([]pbMsg, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 8 {
		return nil, fmt.Errorf("image %s too short (%d bytes)", path, len(data))
	}
	if m := binary.LittleEndian.Uint32(data[0:4]); m != criuImgCommonMagic {
		return nil, fmt.Errorf("image %s: bad common magic %#x", path, m)
	}
	if m := binary.LittleEndian.Uint32(data[4:8]); m != magic {
		return nil, fmt.Errorf("image %s: magic %#x, expected %#x", path, m, magic)
	}
	data = data[8:]
	var out []pbMsg
	for len(data) > 0 {
		if len(data) < 4 {
			return nil, fmt.Errorf("image %s: truncated entry size", path)
		}
		size := binary.LittleEndian.Uint32(data)
		data = data[4:]
		if uint64(size) > uint64(len(data)) {
			return nil, fmt.Errorf("image %s: entry of %d bytes exceeds the image", path, size)
		}
		msg, err := parsePB(data[:size])
		if err != nil {
			return nil, fmt.Errorf("image %s: %w", path, err)
		}
		out = append(out, msg)
		data = data[size:]
	}
	return out, nil
}

// readCriuImageOne returns the single entry of an image such as core-<pid>.
func readCriuImageOne(path string, magic uint32) (pbMsg, error) {
	entries, err := readCriuImage(path, magic)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("image %s has no entry", path)
	}
	return entries[0], nil
}

// --- memory pages ---

// Pagemap entry flags (criu/include/pagemap.h).
const (
	pePARENT         = 1 << 0
	pePRESENT        = 1 << 2
	pePAYLOADALIGNED = 1 << 3
)

var (
	// errPageMissing: the page is not in the images. CRIU leaves out pages it
	// restores from elsewhere, such as unmodified file-backed mappings.
	errPageMissing = errors.New("page not in the images")
	// errPageCompressed: the page is in an LZ4-compressed block, which this
	// reader does not decode.
	errPageCompressed = errors.New("page is in a compressed block")
)

type pagemapEntry struct {
	vaddr  uint64
	nr     uint64
	flags  uint64
	offset int64 // payload offset in pages-<id>.img, for present entries
	// Compressed layout (pagemap_blocks), when hasBlocks.
	hasBlocks     bool
	blockSizes    []uint64
	pagesPerBlock uint64
}

// pageSource reads one process's pages from an image set, following the
// image set's parent link for pages an incremental dump left in the parent.
type pageSource struct {
	dir       string
	pid       int
	entries   []pagemapEntry
	pagesPath string
	parent    *pageSource
	parentErr error
	parentSet bool
}

func openPageSource(dir string, pid int) (*pageSource, error) {
	msgs, err := readCriuImage(filepath.Join(dir, "pagemap-"+strconv.Itoa(pid)+".img"), criuPagemapMagic)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("pagemap of pid %d in %s has no head", pid, dir)
	}
	pagesID, ok := msgs[0].uint(1)
	if !ok {
		return nil, fmt.Errorf("pagemap of pid %d in %s: head without pages_id", pid, dir)
	}
	s := &pageSource{dir: dir, pid: pid, pagesPath: filepath.Join(dir, "pages-"+strconv.FormatUint(pagesID, 10)+".img")}
	var off int64
	for _, m := range msgs[1:] {
		e := pagemapEntry{}
		e.vaddr, _ = m.uint(1)
		if nr, ok := m.uint(5); ok {
			e.nr = nr
		} else {
			e.nr, _ = m.uint(2) // compat_nr_pages
		}
		e.flags, _ = m.uint(4)
		if inParent, ok := m.uint(3); ok && inParent != 0 {
			e.flags |= pePARENT
		}
		blocks, ok, err := m.msg(6)
		if err != nil {
			return nil, fmt.Errorf("pagemap of pid %d in %s: blocks: %w", pid, dir, err)
		}
		var payload uint64
		if ok {
			e.hasBlocks = true
			if e.blockSizes, err = blocks.uints(1); err != nil {
				return nil, err
			}
			payload, _ = blocks.uint(2)
			e.pagesPerBlock, _ = blocks.uint(3)
			if e.pagesPerBlock == 0 {
				return nil, fmt.Errorf("pagemap of pid %d in %s: zero pages_per_block", pid, dir)
			}
		} else {
			payload = e.nr * criuPageSize
		}
		if e.flags&pePRESENT != 0 && e.flags&pePARENT == 0 {
			if e.flags&pePAYLOADALIGNED != 0 && off%criuPageSize != 0 {
				off += criuPageSize - off%criuPageSize
			}
			e.offset = off
			off += int64(payload)
		}
		s.entries = append(s.entries, e)
	}
	return s, nil
}

func (s *pageSource) parentSource() (*pageSource, error) {
	if !s.parentSet {
		s.parentSet = true
		s.parent, s.parentErr = openPageSource(filepath.Join(s.dir, "parent"), s.pid)
	}
	return s.parent, s.parentErr
}

// page returns the 4 KiB page at page-aligned vaddr va.
func (s *pageSource) page(va uint64) ([]byte, error) {
	for i := range s.entries {
		e := &s.entries[i]
		if va < e.vaddr || va >= e.vaddr+e.nr*criuPageSize {
			continue
		}
		idx := (va - e.vaddr) / criuPageSize
		if e.flags&pePARENT != 0 {
			p, err := s.parentSource()
			if err != nil {
				return nil, fmt.Errorf("parent images of %s: %w", s.dir, err)
			}
			return p.page(va)
		}
		if e.flags&pePRESENT == 0 {
			return nil, errPageMissing
		}
		off, raw, err := e.pageOffset(idx)
		if err != nil {
			return nil, err
		}
		if !raw {
			return make([]byte, criuPageSize), nil // all-zero block
		}
		f, err := os.Open(s.pagesPath)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		buf := make([]byte, criuPageSize)
		if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
			return nil, fmt.Errorf("read %s at %d: %w", s.pagesPath, off, err)
		}
		return buf, nil
	}
	return nil, errPageMissing
}

// pageOffset locates page idx of a present entry. raw is false for a page in
// an all-zero block, which has no payload.
func (e *pagemapEntry) pageOffset(idx uint64) (off int64, raw bool, err error) {
	if !e.hasBlocks {
		return e.offset + int64(idx*criuPageSize), true, nil
	}
	off = e.offset
	for b, size := range e.blockSizes {
		first := uint64(b) * e.pagesPerBlock
		n := min(e.pagesPerBlock, e.nr-first)
		if idx >= first+n {
			off += int64(size)
			continue
		}
		switch size {
		case 0:
			return 0, false, nil
		case n * criuPageSize:
			return off + int64((idx-first)*criuPageSize), true, nil
		default:
			return 0, false, errPageCompressed
		}
	}
	return 0, false, fmt.Errorf("page %d beyond the blocks of its pagemap entry", idx)
}

// readRange returns the bytes of [start, end) of the process's memory.
func (s *pageSource) readRange(start, end uint64) ([]byte, error) {
	if end < start || end-start > 1<<20 {
		return nil, fmt.Errorf("range %#x-%#x out of bounds", start, end)
	}
	out := make([]byte, 0, end-start)
	for a := start; a < end; {
		base := a - a%criuPageSize
		pg, err := s.page(base)
		if err != nil {
			return nil, err
		}
		stop := min(end, base+criuPageSize)
		out = append(out, pg[a-base:stop-base]...)
		a = stop
	}
	return out, nil
}
