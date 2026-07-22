package mdf

import (
	"encoding/binary"
	"fmt"

	"github.com/rveen/logb"
	"github.com/rveen/logb/dbc"
	"github.com/rveen/logb/internal/tick"
)

// Decoding a bus recording against a database.
//
// An MDF bus-logging group is frames: a timestamp, an identifier, and a payload.
// It contains no signals, because a recording never does — what the payload
// bytes mean lives in a DBC, and without one there is nothing to plot but the
// identifier. Given one, each message in the database becomes a stream of its
// own beside the raw frames, and "EngineSpeed" becomes a field with a unit, a
// conversion and a bit offset.
//
// The frames are kept either way. A decoded stream is an interpretation, and the
// bytes it was derived from are the evidence for it: throwing them away would
// mean re-importing to check a signal, or to fix a database that turned out to be
// wrong about one.

// canGroup is the bus-logging convention MDF4 uses for CAN: a composed
// CAN_DataFrame channel whose members carry the identifier, the length and the
// payload. The names are from the ASAM bus logging specification and are what
// every logger writes.
type canGroup struct {
	g       *Group
	master  *Channel
	id      *Channel
	ide     *Channel // extended-identifier flag
	dlc     *Channel
	length  *Channel
	payload *Channel
	prefix  string
}

// asCAN reports whether a group is a CAN bus recording, and where its parts are.
func asCAN(g *Group) *canGroup {
	c := &canGroup{g: g, master: g.Master()}
	for _, ch := range g.Channels {
		name := ch.Name
		if i := indexByte(name, '.'); i >= 0 {
			c.prefix, name = name[:i], name[i+1:]
		}
		switch name {
		case "ID":
			c.id = ch
		case "IDE":
			c.ide = ch
		case "DLC":
			c.dlc = ch
		case "DataLength":
			c.length = ch
		case "DataBytes":
			c.payload = ch
		}
	}
	if c.id == nil || c.payload == nil || c.master == nil {
		return nil
	}
	return c
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// decoded is one DBC message that the recording actually contains.
type decoded struct {
	msg     *dbc.Message
	schema  *logb.Schema
	records []byte
	count   int
}

// decode builds a stream per database message present in a CAN group.
//
// Only messages that actually occur get a stream. A vehicle database describes
// every message on every bus of a model range; a recording holds the handful
// that were on this wire, and declaring the rest would bury the four signals
// someone came to look at under two hundred empty ones.
func decode(f *File, c *canGroup, db *dbc.File, exp int8, o *Options) ([]*decoded, error) {
	byKey := map[uint64]*dbc.Message{}
	for _, m := range db.Messages {
		byKey[m.Key()] = m
	}

	// Which messages are here, and how many frames each has, so every buffer is
	// allocated once.
	counts := map[uint64]int{}
	unknown := map[uint32]int{}
	for i := 0; i < c.g.Records; i++ {
		key, id, err := c.frameKey(i)
		if err != nil {
			return nil, err
		}
		if _, ok := byKey[key]; ok {
			counts[key]++
		} else {
			unknown[id]++
		}
	}

	var out []*decoded
	for _, m := range db.Messages {
		n := counts[m.Key()]
		if n == 0 {
			continue
		}
		s, err := dbc.Schema(m, dbc.SchemaOptions{
			Namespace: fmt.Sprintf("logb/mdf/%d/%s", f.StartTime.UnixNano(), c.g.Name),
			Database:  db.Name,
			AxisExp:   exp,
			Warn:      o.Warn,
		})
		if err != nil {
			o.Warn("%v", err)
			continue
		}
		out = append(out, &decoded{
			msg:     m,
			schema:  s,
			records: make([]byte, 0, n*s.RecordBytes()),
		})
	}

	byMsg := map[uint64]*decoded{}
	for _, d := range out {
		byMsg[d.msg.Key()] = d
	}

	for i := 0; i < c.g.Records; i++ {
		key, _, err := c.frameKey(i)
		if err != nil {
			return nil, err
		}
		d := byMsg[key]
		if d == nil {
			continue
		}
		rec := make([]byte, d.schema.RecordBytes())

		t, err := c.master.Float(c.g.Record(i))
		if err != nil {
			return nil, err
		}
		binary.LittleEndian.PutUint64(rec, uint64(tick.Of(t, exp)))

		// The payload goes in as it arrived. A frame shorter than the database
		// says leaves the rest of the record zero, and its signals read as
		// whatever those zero bytes mean — which is why the length is checked
		// below rather than assumed.
		payload := c.g.VLSD[c.payload][i]
		copy(rec[dbc.AxisBits/8:], payload)

		d.records = append(d.records, rec...)
		d.count++
		if len(payload) < d.msg.Length {
			o.Warn("message %s: a frame carried %d bytes, the database says %d",
				d.msg.Name, len(payload), d.msg.Length)
		}
	}

	for id, n := range unknown {
		o.Warn("%d frame(s) with identifier 0x%X are not in the database", n, id)
	}
	return out, nil
}

// frameKey returns the database lookup key of frame i and its raw identifier.
func (c *canGroup) frameKey(i int) (uint64, uint32, error) {
	rec := c.g.Record(i)
	v, err := c.id.Raw(rec)
	if err != nil {
		return 0, 0, err
	}
	id, ok := v.(uint64)
	if !ok {
		return 0, 0, fmt.Errorf("mdf: CAN identifier is %T, not an integer", v)
	}

	// Whether the frame is extended is its own flag; a database distinguishes
	// two messages that share a number and differ only in that. A logger that
	// does not record the flag leaves it to the identifier's width.
	extended := id > 0x7FF
	if c.ide != nil {
		if v, err := c.ide.Raw(rec); err == nil {
			if b, ok := v.(uint64); ok {
				extended = b != 0
			}
		}
	}
	return dbc.Key(uint32(id), extended), uint32(id), nil
}
