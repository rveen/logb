// Package mdf reads ASAM MDF version 4 measurement files.
//
// It exists to feed the importer in this package's Convert, and its shape
// follows from that: it hands back a channel group's records as the bytes the
// file stores, not as decoded samples. An importer wants the record layout —
// Logb keeps it, near enough byte for byte — and decoding every channel to
// float64 on the way in and back to bits on the way out would be work done to
// throw away, and a chance to be wrong.
//
// What it reads: MDF 4.x, sorted and unsorted, uncompressed and deflated (both
// plain and column-transposed), finalized and not, with composed channels,
// variable-length signal data, conversions and attachments. What it does not:
// MDF 3, which is a different container and would be a different parser.
package mdf

import (
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

// Kind is what a channel is for.
type Kind uint8

const (
	// Value is an ordinary measured channel.
	Value Kind = iota

	// VLSD is a channel whose samples are variable-length and live outside the
	// record; the record holds an offset into their stream.
	VLSD

	// Master is the group's independent variable — its time, angle or distance
	// column. MDF calls it a master channel and stores it as one column among
	// the others; Logb calls it the axis and gives it a name in the schema.
	Master

	// VirtualMaster is a master whose value is the record index itself, so it
	// occupies no bytes in the record.
	VirtualMaster
)

// Sync is what a master channel measures.
type Sync uint8

const (
	SyncNone Sync = iota
	SyncTime
	SyncAngle
	SyncDistance
	SyncIndex
)

// MDF data_type codes. Kept as the file's own numbers because the mapping onto
// Logb's seven types is the importer's business, not the reader's.
const (
	DTUintLE uint8 = iota
	DTUintBE
	DTIntLE
	DTIntBE
	DTFloatLE
	DTFloatBE
	DTStringLatin1
	DTStringUTF8
	DTStringUTF16LE
	DTStringUTF16BE
	DTBytes
	DTMimeSample
	DTMimeStream
	DTCANopenDate
	DTCANopenTime
	DTComplexLE
	DTComplexBE
)

// Channel is one CN block: a named column of a channel group's records.
type Channel struct {
	Name string
	Unit string
	Desc string

	Kind Kind
	Sync Sync

	DataType uint8

	// Where the value sits in the record. BitOffset is within the byte at
	// ByteOffset, counted from the least significant bit for the little-endian
	// data types and from the most significant for the big-endian ones — which
	// is also, exactly, how Logb numbers bits.
	ByteOffset uint32
	BitOffset  uint32
	BitCount   uint32

	// InvalBit is this channel's bit in the record's invalidation bytes. A set
	// bit means the sample is not valid, which is Logb's absent field (§6.2).
	InvalBit    uint32
	HasInvalBit bool

	// Conv is the channel's conversion, or nil when it has none and when the
	// file's has no Logb equivalent. It names the MDF conversion either way.
	Conv *Conversion

	// Parent is the composed channel this one is a member of: CAN_DataFrame for
	// CAN_DataFrame.ID. Composed parents are not themselves channels.
	Parent *Channel

	// Array marks a channel whose composition is a CA block. Its bit count
	// covers the whole array, and this reader does not take it apart.
	Array bool

	// vlsdData is where a VLSD channel's samples live: a record id when the
	// group is unsorted, or the address of an SD block when it is not.
	vlsdData int64
}

// BigEndian reports whether the channel's bytes are Motorola-ordered.
func (c *Channel) BigEndian() bool {
	switch c.DataType {
	case DTUintBE, DTIntBE, DTFloatBE, DTStringUTF16BE, DTComplexBE:
		return true
	}
	return false
}

// Group is one channel group: a schema and the records written against it.
type Group struct {
	Name    string // the CG's acquisition name
	Comment string

	// Records is how many records Data holds. It is the file's cycle count when
	// that can be believed and a count of what is actually there when it
	// cannot — an unfinalized file never got its cycle counts written.
	Records int

	// RecordBytes is the fixed part of a record and InvalBytes the invalidation
	// bytes that follow it. Data holds Records × (RecordBytes+InvalBytes) bytes,
	// with the record ids of an unsorted group already stripped.
	RecordBytes int
	InvalBytes  int

	Channels []*Channel
	Data     []byte

	// VLSD holds the resolved samples of each variable-length channel, one
	// entry per record.
	VLSD map[*Channel][][]byte

	recordID uint64
	isVLSD   bool
}

// Record returns record i's bytes, invalidation bytes included.
func (g *Group) Record(i int) []byte {
	n := g.RecordBytes + g.InvalBytes
	return g.Data[i*n : (i+1)*n]
}

// Master returns the group's master channel, or nil when it has none.
func (g *Group) Master() *Channel {
	for _, c := range g.Channels {
		if c.Kind == Master || c.Kind == VirtualMaster {
			return c
		}
	}
	return nil
}

// Attachment is an AT block: a file carried inside the measurement.
type Attachment struct {
	Name     string
	Mime     string
	Comment  string
	Data     []byte
	External bool // stored beside the file rather than in it; Data is empty
}

// File is a parsed MDF4 file.
type File struct {
	Version uint16

	// StartTime is the recording's wall clock, which Logb writes as its
	// segment's start.
	StartTime time.Time

	// Finalized is false for a file whose writer never came back to fill in its
	// cycle counts and block lengths — a logger that was stopped abruptly. Such
	// a file is readable, and this package reads it.
	Finalized bool

	Comment string
	Program string

	Attach []Attachment
	Groups []*Group

	// Events counts EV blocks. They are not converted, and a caller that cares
	// can at least say how many were left behind.
	Events int

	// Transposed reports that the file stored some of its data column-major
	// before compressing it. Logb spells that filter=transpose (§8), and a file
	// whose writer found it worth doing is a file worth doing it to again.
	Transposed bool
}

// ── Reading ───────────────────────────────────────────────────────────────────

type idBlock struct {
	File    [8]byte
	Version [8]byte
	Program [8]byte
	Res     [4]byte
	Ver     uint16
	Res2    [30]byte
	Std     uint16
	Custom  uint16
}

type hdData struct {
	StartNs    int64
	TZMin      int16
	DSTMin     int16
	TimeFlags  uint8
	TimeClass  uint8
	Flags      uint8
	Res        uint8
	StartAngle float64
	StartDist  float64
}

type dgData struct {
	RecIDSize uint8
	Res       [7]byte
}

type cgData struct {
	RecordID   uint64
	CycleCount uint64
	Flags      uint16
	PathSep    uint16
	Res        uint32
	DataBytes  uint32
	InvalBytes uint32
}

type cnData struct {
	ChannelType uint8
	SyncType    uint8
	DataType    uint8
	BitOffset   uint8
	ByteOffset  uint32
	BitCount    uint32
	Flags       uint32
	InvalBitPos uint32
	Precision   uint8
	Res         uint8
	AttachNr    uint16
	MinRaw      float64
	MaxRaw      float64
	LoLimit     float64
	HiLimit     float64
	LoExtLimit  float64
	HiExtLimit  float64
}

type atData struct {
	Flags     uint16
	Creator   uint16
	Res       [4]byte
	MD5       [16]byte
	OrigSize  uint64
	EmbedSize uint64
}

const (
	cgFlagVLSD   uint16 = 1 << 0
	cgFlagRemote uint16 = 1 << 3

	cnFlagInvalValid uint32 = 1 << 1

	atFlagEmbedded   uint16 = 1 << 0
	atFlagCompressed uint16 = 1 << 1
)

// ReadFile parses an MDF4 file. r is read from throughout — the format is a
// graph of file offsets and cannot be streamed — and must stay open and
// unmoved for the life of the returned File only insofar as the caller holds
// slices of it; all data is copied out.
func ReadFile(r io.ReadSeeker) (*File, error) {
	br, err := newReader(r)
	if err != nil {
		return nil, err
	}

	var id idBlock
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.LittleEndian, &id); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotMDF, err)
	}
	switch string(id.File[:4]) {
	case "MDF ", "UnFi": // "MDF     " finalized, "UnFinMF " not
	default:
		return nil, fmt.Errorf("%w (file id %q)", ErrNotMDF, id.File[:])
	}
	if id.Ver < 400 {
		return nil, fmt.Errorf("%w (this file is %d)", ErrVersion, id.Ver)
	}

	f := &File{
		Version:   id.Ver,
		Finalized: id.Std == 0,
		Program:   trimNul(id.Program[:]),
	}

	// The HD block is always at offset 64, the one fixed address in the format.
	_, hl, err := br.expect(64, blkHD)
	if err != nil {
		return nil, err
	}
	var hd hdData
	if err := binary.Read(r, binary.LittleEndian, &hd); err != nil {
		return nil, fmt.Errorf("mdf: header block: %w", err)
	}
	f.StartTime = time.Unix(0, hd.StartNs).UTC().
		Add(time.Duration(hd.TZMin) * time.Minute)
	if len(hl) > 5 {
		if f.Comment, err = br.text(hl[5]); err != nil {
			return nil, fmt.Errorf("mdf: header comment: %w", err)
		}
	}
	if len(hl) > 3 && hl[3] != 0 {
		if f.Attach, err = readAttachments(br, hl[3]); err != nil {
			return nil, err
		}
	}
	if len(hl) > 4 {
		for a := hl[4]; a != 0; {
			b, links, err := br.at(a)
			if err != nil || string(b.ID[:]) != "##EV" || len(links) == 0 {
				break
			}
			f.Events++
			a = links[0]
		}
	}

	for addr := hl[0]; addr != 0; {
		next, groups, err := readDataGroup(br, addr)
		if err != nil {
			return nil, fmt.Errorf("mdf: data group at %d: %w", addr, err)
		}
		f.Groups = append(f.Groups, groups...)
		addr = next
	}
	f.Transposed = br.transposed
	return f, nil
}

func readAttachments(br *reader, addr int64) ([]Attachment, error) {
	var out []Attachment
	for addr != 0 {
		b, links, err := br.expect(addr, blkAT)
		if err != nil {
			return nil, err
		}
		var at atData
		if err := binary.Read(br.r, binary.LittleEndian, &at); err != nil {
			return nil, err
		}
		a := Attachment{External: at.Flags&atFlagEmbedded == 0}
		link := func(i int) int64 {
			if i < len(links) {
				return links[i]
			}
			return 0
		}
		// The payload follows the fixed fields, and is read before the name and
		// mime type — those live in blocks elsewhere in the file, and looking
		// them up moves the reader off the payload.
		if !a.External && at.EmbedSize > 0 {
			at := at // the size fields, captured before any seek
			pos := addr + hdrSize + int64(b.LinkNr)*8 + int64(binary.Size(at))
			if _, err := br.r.Seek(pos, io.SeekStart); err != nil {
				return nil, err
			}
			a.Data = make([]byte, at.EmbedSize)
			if _, err := io.ReadFull(br.r, a.Data); err != nil {
				return nil, fmt.Errorf("mdf: attachment at %d: %w", addr, err)
			}
			if at.Flags&atFlagCompressed != 0 {
				if a.Data, err = inflate(a.Data, at.OrigSize); err != nil {
					return nil, fmt.Errorf("mdf: attachment at %d: %w", addr, err)
				}
			}
		}
		if a.Name, err = br.text(link(1)); err != nil {
			return nil, err
		}
		if a.Mime, err = br.text(link(2)); err != nil {
			return nil, err
		}
		if a.Comment, err = br.text(link(3)); err != nil {
			return nil, err
		}
		out = append(out, a)
		addr = link(0)
	}
	return out, nil
}

// readDataGroup parses one DG block and returns the groups it holds, with their
// records already separated out.
func readDataGroup(br *reader, addr int64) (next int64, groups []*Group, err error) {
	_, links, err := br.expect(addr, blkDG)
	if err != nil {
		return 0, nil, err
	}
	var dg dgData
	if err := binary.Read(br.r, binary.LittleEndian, &dg); err != nil {
		return 0, nil, err
	}
	link := func(i int) int64 {
		if i < len(links) {
			return links[i]
		}
		return 0
	}
	next = link(0)

	for cg := link(1); cg != 0; {
		g, nextCG, err := readChannelGroup(br, cg)
		if err != nil {
			return 0, nil, fmt.Errorf("channel group at %d: %w", cg, err)
		}
		groups = append(groups, g)
		cg = nextCG
	}

	data, err := br.data(link(2))
	if err != nil {
		return 0, nil, err
	}

	if dg.RecIDSize == 0 {
		if len(groups) > 0 {
			g := groups[0]
			g.Data = data
			g.Records = recordCount(len(data), g.RecordBytes+g.InvalBytes, g.Records)
			g.Data = g.Data[:g.Records*(g.RecordBytes+g.InvalBytes)]
		}
	} else if err := demux(data, int(dg.RecIDSize), groups); err != nil {
		return 0, nil, err
	}

	// A sorted group's variable-length samples hang off the channel itself; an
	// unsorted one's were just demuxed into a VLSD group.
	if err := resolveVLSD(br, groups); err != nil {
		return 0, nil, err
	}

	// VLSD groups are stores, not measurements: their records are other
	// channels' samples and they have no schema of their own.
	kept := groups[:0]
	for _, g := range groups {
		if !g.isVLSD {
			kept = append(kept, g)
		}
	}
	return next, kept, nil
}

// recordCount decides how many records a buffer holds. The cycle count wins
// when it is present and fits; when it is zero (an unfinalized file) or longer
// than the data (a truncated one), what is actually there wins.
func recordCount(bytes, size, cycles int) int {
	if size <= 0 {
		return 0
	}
	have := bytes / size
	if cycles > 0 && cycles < have {
		return cycles
	}
	return have
}

func readChannelGroup(br *reader, addr int64) (*Group, int64, error) {
	_, links, err := br.expect(addr, blkCG)
	if err != nil {
		return nil, 0, err
	}
	var cg cgData
	if err := binary.Read(br.r, binary.LittleEndian, &cg); err != nil {
		return nil, 0, err
	}
	link := func(i int) int64 {
		if i < len(links) {
			return links[i]
		}
		return 0
	}
	g := &Group{
		Records:     int(cg.CycleCount),
		RecordBytes: int(cg.DataBytes),
		InvalBytes:  int(cg.InvalBytes),
		recordID:    cg.RecordID,
		isVLSD:      cg.Flags&cgFlagVLSD != 0,
		VLSD:        map[*Channel][][]byte{},
	}
	if g.Name, err = br.text(link(2)); err != nil {
		return nil, 0, err
	}
	if g.Comment, err = br.text(link(5)); err != nil {
		return nil, 0, err
	}
	if cg.Flags&cgFlagRemote != 0 {
		return nil, 0, fmt.Errorf("%w: channel group %q has a remote master", ErrUnsupported, g.Name)
	}

	for cn := link(1); cn != 0; {
		chans, nextCN, err := readChannel(br, cn, nil)
		if err != nil {
			return nil, 0, fmt.Errorf("channel at %d: %w", cn, err)
		}
		g.Channels = append(g.Channels, chans...)
		cn = nextCN
	}
	return g, link(0), nil
}

// readChannel parses one CN block and, when it is a composed channel, the
// members underneath it. A composed parent is dropped: its members cover the
// same bytes with names that already say who they belong to
// (CAN_DataFrame.ID), and keeping both would mean two fields over one value.
func readChannel(br *reader, addr int64, parent *Channel) ([]*Channel, int64, error) {
	_, links, err := br.expect(addr, blkCN)
	if err != nil {
		return nil, 0, err
	}
	var cn cnData
	if err := binary.Read(br.r, binary.LittleEndian, &cn); err != nil {
		return nil, 0, err
	}
	link := func(i int) int64 {
		if i < len(links) {
			return links[i]
		}
		return 0
	}

	c := &Channel{
		DataType:    cn.DataType,
		ByteOffset:  cn.ByteOffset,
		BitOffset:   uint32(cn.BitOffset),
		BitCount:    cn.BitCount,
		InvalBit:    cn.InvalBitPos,
		HasInvalBit: cn.Flags&cnFlagInvalValid != 0,
		Parent:      parent,
	}
	switch cn.ChannelType {
	case 1:
		c.Kind = VLSD
	case 2:
		c.Kind = Master
	case 3:
		c.Kind = VirtualMaster
	default:
		c.Kind = Value
	}
	if cn.SyncType <= uint8(SyncIndex) {
		c.Sync = Sync(cn.SyncType)
	}
	if c.Name, err = br.text(link(2)); err != nil {
		return nil, 0, err
	}
	if c.Unit, err = br.text(link(6)); err != nil {
		return nil, 0, err
	}
	if c.Desc, err = br.text(link(7)); err != nil {
		return nil, 0, err
	}
	if a := link(4); a != 0 {
		if c.Conv, err = readConversion(br, a); err != nil {
			return nil, 0, fmt.Errorf("conversion of %q: %w", c.Name, err)
		}
	}
	if c.Kind == VLSD {
		c.vlsdData = link(5)
	}

	out := []*Channel{c}
	if comp := link(1); comp != 0 {
		kind, err := br.kind(comp)
		if err != nil {
			return nil, 0, err
		}
		switch kind {
		case blkCN:
			// A structure: this channel is a container and its members are the
			// real ones.
			out = nil
			for a := comp; a != 0; {
				members, nextCN, err := readChannel(br, a, c)
				if err != nil {
					return nil, 0, err
				}
				out = append(out, members...)
				a = nextCN
			}
		case blkCA:
			c.Array = true
		}
	}
	return out, link(0), nil
}

// demux splits an unsorted data group's records by their record id.
//
// This is the case §10 of the Logb spec is about: MDF interleaves several
// groups' records in one block and tags each with an id, and every reader pays
// for it on every read. Logb's answer is that the interleaving is a writer's
// problem, so it is undone here, once.
func demux(data []byte, idSize int, groups []*Group) error {
	byID := make(map[uint64]*Group, len(groups))
	for _, g := range groups {
		byID[g.recordID] = g
	}

	// Walked twice: once to size each group's buffer, once to fill it. The
	// alternative is appending record by record, and an hour of CAN traffic is
	// a hundred thousand of them.
	sizes := map[*Group]int{}
	err := walkRecords(data, idSize, byID, func(g *Group, rec []byte) {
		sizes[g] += len(rec)
	})
	if err != nil {
		return err
	}
	for g, n := range sizes {
		g.Data = make([]byte, 0, n)
		g.Records = 0
	}
	return walkRecords(data, idSize, byID, func(g *Group, rec []byte) {
		g.Data = append(g.Data, rec...)
		g.Records++
	})
}

// walkRecords calls fn for every record in an unsorted data block.
//
// It stops without complaint at a record that runs off the end: that is what a
// file whose logger was stopped mid-write looks like, and the records before it
// are perfectly good. It does complain about a record id it has no group for,
// because from there the walk cannot know how long the record is and every
// record after it would be invented.
func walkRecords(data []byte, idSize int, byID map[uint64]*Group, fn func(*Group, []byte)) error {
	for pos := 0; pos+idSize <= len(data); {
		id := recordID(data[pos:], idSize)
		g, ok := byID[id]
		if !ok {
			return fmt.Errorf("%w: record id %d at byte %d", ErrUnsupported, id, pos)
		}
		pos += idSize

		n := g.RecordBytes + g.InvalBytes
		if g.isVLSD {
			// A variable-length record: a u32 length, then that many bytes. The
			// prefix is kept, because the offsets stored in the fixed records
			// are counted from it.
			if pos+4 > len(data) {
				return nil
			}
			n = 4 + int(binary.LittleEndian.Uint32(data[pos:]))
		}
		if pos+n > len(data) {
			return nil
		}
		fn(g, data[pos:pos+n])
		pos += n
	}
	return nil
}

func recordID(b []byte, size int) uint64 {
	switch size {
	case 1:
		return uint64(b[0])
	case 2:
		return uint64(binary.LittleEndian.Uint16(b))
	case 4:
		return uint64(binary.LittleEndian.Uint32(b))
	case 8:
		return binary.LittleEndian.Uint64(b)
	}
	return 0
}

// resolveVLSD turns each VLSD channel's stored offsets into the samples they
// point at, so that nothing downstream has to know the indirection existed.
func resolveVLSD(br *reader, groups []*Group) error {
	byID := map[uint64][]byte{}
	for _, g := range groups {
		if g.isVLSD {
			byID[g.recordID] = g.Data
		}
	}
	for _, g := range groups {
		if g.isVLSD {
			continue
		}
		for _, c := range g.Channels {
			if c.Kind != VLSD {
				continue
			}
			stream, err := vlsdStream(br, c, byID)
			if err != nil {
				return err
			}
			if c.BitCount != 64 || c.BitOffset != 0 {
				return fmt.Errorf("%w: variable-length channel %q is %d bits at bit %d, want a 64-bit offset",
					ErrUnsupported, c.Name, c.BitCount, c.BitOffset)
			}
			size := g.RecordBytes + g.InvalBytes
			samples := make([][]byte, g.Records)
			for i := 0; i < g.Records; i++ {
				rec := g.Data[i*size:]
				if int(c.ByteOffset)+8 > size {
					return fmt.Errorf("%w: channel %q reaches past its record", ErrUnsupported, c.Name)
				}
				off := binary.LittleEndian.Uint64(rec[c.ByteOffset:])
				s, err := vlsdAt(stream, off)
				if err != nil {
					return fmt.Errorf("channel %q record %d: %w", c.Name, i, err)
				}
				samples[i] = s
			}
			g.VLSD[c] = samples
		}
	}
	return nil
}

// vlsdStream finds where a channel's variable-length samples are: an SD block
// of its own, or the records of a VLSD channel group in the same data group.
func vlsdStream(br *reader, c *Channel, byID map[uint64][]byte) ([]byte, error) {
	if c.vlsdData == 0 {
		return nil, nil
	}
	kind, err := br.kind(c.vlsdData)
	if err != nil {
		return nil, err
	}
	if kind == blkCG {
		// The link points at the VLSD channel group whose records are the
		// samples. It was demuxed already; find it by record id.
		if _, _, err := br.expect(c.vlsdData, blkCG); err != nil {
			return nil, err
		}
		var cg cgData
		if err := binary.Read(br.r, binary.LittleEndian, &cg); err != nil {
			return nil, err
		}
		return byID[cg.RecordID], nil
	}
	return br.data(c.vlsdData)
}

// vlsdAt reads the sample at a byte offset into a signal data stream: a u32
// length followed by that many bytes.
func vlsdAt(stream []byte, off uint64) ([]byte, error) {
	if off+4 > uint64(len(stream)) {
		return nil, fmt.Errorf("%w: sample offset %d past %d bytes of signal data",
			ErrUnsupported, off, len(stream))
	}
	n := uint64(binary.LittleEndian.Uint32(stream[off:]))
	if off+4+n > uint64(len(stream)) {
		return nil, fmt.Errorf("%w: sample at %d claims %d bytes, %d remain",
			ErrUnsupported, off, n, uint64(len(stream))-off-4)
	}
	out := make([]byte, n)
	copy(out, stream[off+4:off+4+n])
	return out, nil
}

func trimNul(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
