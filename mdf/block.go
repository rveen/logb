package mdf

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
)

// An MDF4 file is a graph of typed blocks joined by absolute file offsets. Every
// block starts with the same 24-byte header, then its link addresses, then its
// data section — so a reader that knows the header can skip a block type it has
// never heard of, which is the one thing the format got right about forward
// compatibility.
//
// Layouts here were checked against asammdf's v4_blocks.py and against the
// fixtures in testdata/mdf.

const hdrSize = 24 // id[4] + reserved[4] + length[8] + link_count[8]

const (
	blkHD = "##HD"
	blkAT = "##AT"
	blkDG = "##DG"
	blkCG = "##CG"
	blkCN = "##CN"
	blkCC = "##CC"
	blkCA = "##CA"
	blkTX = "##TX"
	blkMD = "##MD"
	blkDT = "##DT"
	blkDL = "##DL"
	blkDZ = "##DZ"
	blkHL = "##HL"
	blkSD = "##SD"
	blkRD = "##RD"
	blkDV = "##DV"
)

// Errors an importer can act on, rather than a string it can only print.
var (
	// ErrNotMDF is returned when the file does not start with an MDF
	// identification block.
	ErrNotMDF = fmt.Errorf("mdf: not an MDF file")

	// ErrVersion is returned for MDF 3 and earlier. Those are a different
	// container — two-byte block ids, no "##" magic, a different link layout —
	// and this package does not pretend to read them.
	ErrVersion = fmt.Errorf("mdf: only MDF 4.x is supported")

	// ErrUnsupported marks a construct this reader refuses rather than guesses
	// at: an unknown compression scheme, an unknown record id.
	ErrUnsupported = fmt.Errorf("mdf: unsupported")
)

// block is the common 24-byte header.
type block struct {
	ID     [4]byte
	Res    [4]byte
	Length uint64
	LinkNr uint64
}

// reader adds block navigation to a ReadSeeker.
type reader struct {
	r    io.ReadSeeker
	size int64

	// transposed records that some data block was stored column-major. It is
	// the file telling us something about its own contents that is worth
	// passing on: data that compressed well that way will compress well that
	// way again (§8).
	transposed bool
}

func newReader(r io.ReadSeeker) (*reader, error) {
	size, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	return &reader{r: r, size: size}, nil
}

// at reads the header and links of the block at addr, leaving the reader
// positioned at the start of the block's data section.
func (br *reader) at(addr int64) (block, []int64, error) {
	var b block
	if addr < 0 || addr >= br.size {
		return b, nil, fmt.Errorf("mdf: block address %d outside the file (%d bytes)", addr, br.size)
	}
	if _, err := br.r.Seek(addr, io.SeekStart); err != nil {
		return b, nil, err
	}
	if err := binary.Read(br.r, binary.LittleEndian, &b); err != nil {
		return b, nil, err
	}
	// A link count is 8 bytes each and cannot exceed the block; without this a
	// corrupt count asks for an allocation the size of the machine.
	if b.LinkNr > (b.Length-hdrSize)/8 || b.Length < hdrSize {
		return b, nil, fmt.Errorf("mdf: block at %d claims %d links in %d bytes", addr, b.LinkNr, b.Length)
	}
	links := make([]int64, b.LinkNr)
	if err := binary.Read(br.r, binary.LittleEndian, links); err != nil {
		return b, nil, err
	}
	return b, links, nil
}

// expect is at, with the block type checked.
func (br *reader) expect(addr int64, id string) (block, []int64, error) {
	b, links, err := br.at(addr)
	if err != nil {
		return b, links, err
	}
	if got := string(b.ID[:]); got != id {
		return b, links, fmt.Errorf("mdf: expected %s at %d, found %q", id, addr, got)
	}
	return b, links, nil
}

// kind returns a block's four-byte id without reading the rest of it.
func (br *reader) kind(addr int64) (string, error) {
	if _, err := br.r.Seek(addr, io.SeekStart); err != nil {
		return "", err
	}
	var id [4]byte
	if _, err := io.ReadFull(br.r, id[:]); err != nil {
		return "", err
	}
	return string(id[:]), nil
}

// dataOf reads the data section of the block whose header and links have just
// been read, from wherever the reader is now.
func (br *reader) dataOf(b block) ([]byte, error) {
	n := int64(b.Length) - hdrSize - int64(b.LinkNr)*8
	if n <= 0 {
		return nil, nil
	}
	buf := make([]byte, n)
	_, err := io.ReadFull(br.r, buf)
	return buf, err
}

// text reads a TX or MD block: a null-terminated UTF-8 string. Address 0 is the
// format's "no text", and is not an error.
func (br *reader) text(addr int64) (string, error) {
	if addr == 0 {
		return "", nil
	}
	b, _, err := br.at(addr)
	if err != nil {
		return "", err
	}
	if id := string(b.ID[:]); id != blkTX && id != blkMD {
		return "", fmt.Errorf("mdf: expected %s/%s at %d, found %q", blkTX, blkMD, addr, id)
	}
	buf, err := br.dataOf(b)
	if err != nil {
		return "", err
	}
	if i := bytes.IndexByte(buf, 0); i >= 0 {
		buf = buf[:i]
	}
	return string(buf), nil
}

// ── Data blocks ───────────────────────────────────────────────────────────────

// data assembles the bytes a DG or a VLSD channel points at, following the DL
// lists and HL headers and decompressing DZ blocks on the way.
//
// The whole thing is materialised in memory. That is a real limit — a gigabyte
// recording needs a gigabyte — and it is the honest shape for an importer, which
// is going to touch every byte exactly once anyway.
func (br *reader) data(addr int64) ([]byte, error) {
	if addr == 0 {
		return nil, nil
	}
	id, err := br.kind(addr)
	if err != nil {
		return nil, err
	}
	switch id {
	case blkDT, blkSD, blkRD, blkDV:
		return br.plainData(addr)
	case blkDL:
		return br.listData(addr)
	case blkDZ:
		return br.zipData(addr)
	case blkHL:
		_, links, err := br.expect(addr, blkHL)
		if err != nil || len(links) == 0 {
			return nil, err
		}
		return br.listData(links[0])
	default:
		return nil, fmt.Errorf("%w: data block type %q at %d", ErrUnsupported, id, addr)
	}
}

// plainData reads an uncompressed data block.
//
// A DT block whose length is still the placeholder is not corruption: an
// unfinalized file — a logger that lost power, or that was stopped without the
// courtesy of a footer — never went back to patch it. The data runs to the end
// of the file, which is exactly what the standard's "update of last DT block
// length required" flag says it does.
func (br *reader) plainData(addr int64) ([]byte, error) {
	b, links, err := br.at(addr)
	if err != nil {
		return nil, err
	}
	start := addr + hdrSize + int64(len(links))*8
	n := int64(b.Length) - hdrSize - int64(b.LinkNr)*8
	if n <= 0 {
		n = br.size - start
	}
	if n <= 0 {
		return nil, nil
	}
	if start+n > br.size {
		n = br.size - start
	}
	if _, err := br.r.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(br.r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// listData concatenates every block a DL chain points at.
func (br *reader) listData(addr int64) ([]byte, error) {
	var out []byte
	for addr != 0 {
		_, links, err := br.expect(addr, blkDL)
		if err != nil {
			return nil, err
		}
		if len(links) == 0 {
			return out, nil
		}
		next, blocks := links[0], links[1:]
		for i, a := range blocks {
			if a == 0 {
				continue
			}
			chunk, err := br.data(a)
			if err != nil {
				return nil, fmt.Errorf("mdf: data list entry %d at %d: %w", i, a, err)
			}
			out = append(out, chunk...)
		}
		addr = next
	}
	return out, nil
}

// zipHeader follows the common header of a DZ block.
type zipHeader struct {
	OrigType [2]byte
	ZipType  uint8
	Res      uint8
	Param    uint32
	OrigSize uint64
	ZipSize  uint64
}

func (br *reader) zipData(addr int64) ([]byte, error) {
	if _, _, err := br.expect(addr, blkDZ); err != nil {
		return nil, err
	}
	var z zipHeader
	if err := binary.Read(br.r, binary.LittleEndian, &z); err != nil {
		return nil, err
	}
	packed := make([]byte, z.ZipSize)
	if _, err := io.ReadFull(br.r, packed); err != nil {
		return nil, err
	}
	out, err := inflate(packed, z.OrigSize)
	if err != nil {
		return nil, fmt.Errorf("mdf: data block at %d: %w", addr, err)
	}

	switch z.ZipType {
	case 0: // deflate
		return out, nil
	case 1: // deflate over column-major bytes
		br.transposed = true
		return detranspose(out, int(z.Param)), nil
	default:
		return nil, fmt.Errorf("%w: DZ zip_type %d at %d", ErrUnsupported, z.ZipType, addr)
	}
}

// inflate decompresses n bytes of zlib-wrapped deflate, the one compression
// MDF4 uses — for data blocks and for embedded attachments alike.
func inflate(packed []byte, n uint64) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(packed))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	out := make([]byte, n)
	if _, err := io.ReadFull(zr, out); err != nil {
		return nil, err
	}
	return out, nil
}

// detranspose undoes the column-major shuffle MDF applies before deflating —
// the same trick Logb spells filter=transpose (§8), and the one genuinely good
// idea this part of MDF has: byte i of every record ends up adjacent, and a
// column that barely changes compresses to nothing.
func detranspose(data []byte, cols int) []byte {
	if cols <= 0 || len(data) == 0 || len(data)%cols != 0 {
		return data
	}
	rows := len(data) / cols
	out := make([]byte, len(data))
	for r := 0; r < rows; r++ {
		for c := 0; c < cols; c++ {
			out[r*cols+c] = data[c*rows+r]
		}
	}
	return out
}
