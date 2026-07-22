// Package dbc reads Vector DBC databases and maps them onto Logb schemas.
//
// A CAN recording is frames: an identifier and eight bytes. What those bytes
// mean — that bits 24 to 39 of message 0x100 are engine speed in quarter-rpm —
// is not in the recording and never was. It is in a database, and without one
// no tool can show a signal, because there is no signal to show.
//
// This package is the front half of that. It parses the database and turns each
// message into a Logb schema whose fields sit at the bit offsets the DBC
// specifies, so that decoding a frame is reading a record: no per-signal shift
// and mask at display time, and no second implementation of the bit rule to get
// wrong. See CAN.md.
package dbc

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// A File is a parsed database.
type File struct {
	Version  string
	Nodes    []string
	Messages []*Message

	// Name and Raw are the database as it arrived, kept so that an importer can
	// carry it into the file it writes. A converted recording that says what
	// every signal means but not which database said so is a document with its
	// citation removed: the signals are checkable only against a file somebody
	// has to still have.
	Name string
	Raw  []byte
}

// SHA256 is the hex digest of the database's bytes, or "" if it was parsed from
// a source that did not keep them. Two recordings decoded with databases of the
// same name and different contents are the reason this is worth recording.
func (f *File) SHA256() string {
	if len(f.Raw) == 0 {
		return ""
	}
	sum := sha256.Sum256(f.Raw)
	return hex.EncodeToString(sum[:])
}

// Message is one CAN frame layout: a BO_ entry.
type Message struct {
	ID       uint32 // 11 or 29 bits, without the extended marker
	Extended bool
	Name     string
	Length   int // bytes, the DLC
	Sender   string
	Desc     string
	Signals  []*Signal
}

// Signal is one field of a message: an SG_ entry.
type Signal struct {
	Name string

	// Start is the DBC start bit, exactly as written. It is *not* a Logb bit
	// offset — for a Motorola signal the two differ. Use BitOffset.
	Start  int
	Length int

	// BigEndian is DBC's @0, which Vector's documentation calls Motorola.
	BigEndian bool
	Signed    bool

	// Float is set by SIG_VALTYPE_: the raw bits are an IEEE float rather than
	// an integer, and Length is 32 or 64.
	Float bool

	Factor, Offset float64
	Min, Max       float64
	Unit           string
	Receivers      []string
	Desc           string

	// Multiplexor marks the signal that selects which of the multiplexed
	// signals are present in a frame — DBC's "M".
	Multiplexor bool

	// Muxed marks a signal present only when the multiplexor holds MuxValue —
	// DBC's "m<n>".
	Muxed    bool
	MuxValue uint64

	// Values is a VAL_ enumeration: raw value to name.
	Values map[uint64]string

	// ExtendedMux records that this signal came with an SG_MUL_VAL_ entry
	// naming more than one multiplexor value, or a multiplexor that is itself
	// multiplexed. Logb's guards do not chain and hold one value (SPEC §6.2),
	// so such a signal cannot be expressed and Schema refuses it rather than
	// decoding it in frames where it is not present.
	ExtendedMux bool
}

// BitOffset is the signal's first bit in Logb's numbering.
//
// For an Intel signal the two numberings coincide. For a Motorola signal this
// is the whole of the conversion, and it is the claim CAN.md rests on: Logb's
// big-endian bit numbering *is* DBC's Motorola convention, so a signal that
// crosses a byte boundary — the case MDF4 leaves undefined — needs no walk, no
// special case, and no data movement. logb's TestDBCMotorola checks this
// formula against Vector's reference algorithm over 465,600 cases.
func (s *Signal) BitOffset() uint32 {
	if !s.BigEndian {
		return uint32(s.Start)
	}
	return uint32(8*(s.Start/8) + (7 - s.Start%8))
}

// Key identifies a message for lookup against a received frame.
func Key(id uint32, extended bool) uint64 {
	k := uint64(id)
	if extended {
		k |= 1 << 32
	}
	return k
}

// Key is the message's own lookup key.
func (m *Message) Key() uint64 { return Key(m.ID, m.Extended) }

// Multiplexor returns the message's multiplexor signal, or nil.
func (m *Message) Multiplexor() *Signal {
	for _, s := range m.Signals {
		if s.Multiplexor {
			return s
		}
	}
	return nil
}

// extendedID is the bit a DBC sets in a message id to mark a 29-bit frame.
const extendedID = 1 << 31

var (
	msgRe = regexp.MustCompile(`^BO_\s+(\d+)\s+([^\s:]+)\s*:\s*(\d+)\s*(\S*)`)

	// SG_ <name> [M|m<n>[M]] : <start>|<len>@<order><sign> (<factor>,<offset>) [<min>|<max>] "<unit>" <receivers>
	sigRe = regexp.MustCompile(`^\s*SG_\s+([^\s:]+)\s*(M|m\d+M?)?\s*:\s*` +
		`(\d+)\|(\d+)@([01])([-+])\s*\(([^,]*),([^)]*)\)\s*` +
		`\[([^|\]]*)\|([^\]]*)\]\s*"([^"]*)"\s*(.*)$`)

	valRe      = regexp.MustCompile(`^VAL_\s+(\d+)\s+([^\s]+)\s+(.*?);?\s*$`)
	valPairRe  = regexp.MustCompile(`(-?\d+)\s+"([^"]*)"`)
	valTypeRe  = regexp.MustCompile(`^SIG_VALTYPE_\s+(\d+)\s+([^\s]+)\s*:?\s*(\d+)`)
	msgCmtRe   = regexp.MustCompile(`^CM_\s+BO_\s+(\d+)\s+"((?s).*)"\s*;?\s*$`)
	sigCmtRe   = regexp.MustCompile(`^CM_\s+SG_\s+(\d+)\s+([^\s]+)\s+"((?s).*)"\s*;?\s*$`)
	mulValRe   = regexp.MustCompile(`^SG_MUL_VAL_\s+(\d+)\s+([^\s]+)\s+([^\s]+)\s+(.*?);?\s*$`)
	mulRangeRe = regexp.MustCompile(`(\d+)-(\d+)`)
)

// ParseFile reads a database from a path, keeping its bytes and base name so
// that an importer can embed it.
func ParseFile(path string) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	d, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	d.Name = filepath.Base(path)
	return d, nil
}

// Parse reads a database.
//
// Entries this package does not model — attribute definitions, environment
// variables, node lists beyond their names — are skipped rather than refused: a
// real DBC is full of tooling metadata that says nothing about the wire, and an
// importer that stopped at the first BA_DEF_ would read almost no real file.
func Parse(r io.Reader) (*File, error) {
	// The bytes are kept, not just the parse: a database is small, and an
	// importer that embeds it lets a reader check a signal against the
	// definition it was decoded with rather than one that has the same name.
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	d := &File{Raw: raw}
	byID := map[uint32]*Message{}

	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var msg *Message   // the message SG_ lines attach to
	var pending string // a statement being accumulated across lines
	line := 0

	for sc.Scan() {
		line++
		text := strings.TrimRight(sc.Text(), "\r")

		// A comment or a value table may run over several lines. Anything that
		// opens a quote without closing it is continued until it does.
		if pending != "" {
			pending += "\n" + text
			if !complete(pending) {
				continue
			}
			text, pending = pending, ""
		} else if trimmed := strings.TrimSpace(text); isStatement(trimmed) && !complete(text) {
			pending = text
			continue
		}

		trimmed := strings.TrimSpace(text)
		switch {
		case strings.HasPrefix(trimmed, "VERSION"):
			d.Version = unquote(strings.TrimSpace(trimmed[len("VERSION"):]))

		case strings.HasPrefix(trimmed, "BU_:"):
			d.Nodes = strings.Fields(trimmed[len("BU_:"):])

		case strings.HasPrefix(trimmed, "BO_ "):
			m, err := parseMessage(trimmed)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", line, err)
			}
			d.Messages = append(d.Messages, m)
			byID[m.ID] = m
			msg = m

		case strings.HasPrefix(trimmed, "SG_ "):
			if msg == nil {
				return nil, fmt.Errorf("line %d: signal outside a message", line)
			}
			s, err := parseSignal(text)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", line, err)
			}
			msg.Signals = append(msg.Signals, s)

		case strings.HasPrefix(trimmed, "VAL_ "):
			if err := parseValues(trimmed, byID); err != nil {
				return nil, fmt.Errorf("line %d: %w", line, err)
			}

		case strings.HasPrefix(trimmed, "SIG_VALTYPE_ "):
			if m := valTypeRe.FindStringSubmatch(trimmed); m != nil {
				if s := signalOf(byID, m[1], m[2]); s != nil {
					switch m[3] {
					case "1":
						s.Float, s.Length = true, 32
					case "2":
						s.Float, s.Length = true, 64
					}
				}
			}

		case strings.HasPrefix(trimmed, "CM_ "):
			parseComment(trimmed, byID)

		case strings.HasPrefix(trimmed, "SG_MUL_VAL_ "):
			parseExtendedMux(trimmed, byID)

			// Everything else — BS_, NS_, BA_, BA_DEF_, EV_, blank lines, the
			// continuation lines of the NS_ block — says nothing about the wire
			// and is skipped.
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(d.Messages) == 0 {
		return nil, fmt.Errorf("dbc: no messages found; is this a DBC file?")
	}
	return d, nil
}

// isStatement reports whether a line begins a DBC statement that may carry a
// quoted string over several lines.
func isStatement(s string) bool {
	for _, k := range []string{"CM_ ", "VAL_ ", "BA_ ", "BA_DEF_ ", "BA_DEF_DEF_ "} {
		if strings.HasPrefix(s, k) {
			return true
		}
	}
	return false
}

// complete reports whether a statement's quotes are balanced.
func complete(s string) bool { return strings.Count(s, `"`)%2 == 0 }

func parseMessage(s string) (*Message, error) {
	m := msgRe.FindStringSubmatch(s)
	if m == nil {
		return nil, fmt.Errorf("malformed BO_: %q", s)
	}
	id, err := strconv.ParseUint(m[1], 10, 32)
	if err != nil {
		return nil, fmt.Errorf("bad message id %q: %w", m[1], err)
	}
	length, _ := strconv.Atoi(m[3])
	return &Message{
		ID:       uint32(id) &^ extendedID,
		Extended: uint32(id)&extendedID != 0,
		Name:     m[2],
		Length:   length,
		Sender:   m[4],
	}, nil
}

func parseSignal(s string) (*Signal, error) {
	m := sigRe.FindStringSubmatch(s)
	if m == nil {
		return nil, fmt.Errorf("malformed SG_: %q", strings.TrimSpace(s))
	}
	sg := &Signal{
		Name:      m[1],
		BigEndian: m[5] == "0",
		Signed:    m[6] == "-",
		Unit:      m[11],
		Receivers: strings.FieldsFunc(m[12], func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }),
	}
	sg.Start, _ = strconv.Atoi(m[3])
	sg.Length, _ = strconv.Atoi(m[4])
	sg.Factor = num(m[7], 1)
	sg.Offset = num(m[8], 0)
	sg.Min = num(m[9], 0)
	sg.Max = num(m[10], 0)

	switch mux := m[2]; {
	case mux == "M":
		sg.Multiplexor = true
	case strings.HasPrefix(mux, "m"):
		v, err := strconv.ParseUint(strings.TrimSuffix(mux[1:], "M"), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("bad multiplexor value %q", mux)
		}
		sg.Muxed, sg.MuxValue = true, v
		if strings.HasSuffix(mux, "M") {
			// Multiplexed *and* a multiplexor: a second level, which is the one
			// shape Logb's guards cannot express.
			sg.Multiplexor, sg.ExtendedMux = true, true
		}
	}
	return sg, nil
}

func parseValues(s string, byID map[uint32]*Message) error {
	m := valRe.FindStringSubmatch(s)
	if m == nil {
		return nil // VAL_ on an environment variable rather than a signal
	}
	sig := signalOf(byID, m[1], m[2])
	if sig == nil {
		return nil
	}
	sig.Values = map[uint64]string{}
	for _, pair := range valPairRe.FindAllStringSubmatch(m[3], -1) {
		v, err := strconv.ParseInt(pair[1], 10, 64)
		if err != nil {
			continue
		}
		sig.Values[uint64(v)] = pair[2]
	}
	return nil
}

func parseComment(s string, byID map[uint32]*Message) {
	if m := sigCmtRe.FindStringSubmatch(s); m != nil {
		if sig := signalOf(byID, m[1], m[2]); sig != nil {
			sig.Desc = m[3]
		}
		return
	}
	if m := msgCmtRe.FindStringSubmatch(s); m != nil {
		if id, err := strconv.ParseUint(m[1], 10, 32); err == nil {
			if msg := byID[uint32(id)&^extendedID]; msg != nil {
				msg.Desc = m[2]
			}
		}
	}
}

// parseExtendedMux reads an SG_MUL_VAL_ entry, which extends multiplexing to
// several values per signal and to multiplexors that are themselves
// multiplexed. A single value of a single range is ordinary multiplexing said
// differently and is taken; anything else is marked and refused later, because
// a signal decoded in frames that do not contain it returns plausible garbage
// — the failure mode §6.2 exists to prevent.
func parseExtendedMux(s string, byID map[uint32]*Message) {
	m := mulValRe.FindStringSubmatch(s)
	if m == nil {
		return
	}
	sig := signalOf(byID, m[1], m[2])
	if sig == nil {
		return
	}
	ranges := mulRangeRe.FindAllStringSubmatch(m[4], -1)
	if len(ranges) != 1 {
		sig.ExtendedMux = true
		return
	}
	lo, _ := strconv.ParseUint(ranges[0][1], 10, 64)
	hi, _ := strconv.ParseUint(ranges[0][2], 10, 64)
	if lo != hi {
		sig.ExtendedMux = true
		return
	}
	sig.Muxed, sig.MuxValue = true, lo
}

func signalOf(byID map[uint32]*Message, id, name string) *Signal {
	n, err := strconv.ParseUint(id, 10, 32)
	if err != nil {
		return nil
	}
	msg := byID[uint32(n)&^extendedID]
	if msg == nil {
		return nil
	}
	for _, s := range msg.Signals {
		if s.Name == name {
			return s
		}
	}
	return nil
}

func num(s string, def float64) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return def
	}
	return v
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}
