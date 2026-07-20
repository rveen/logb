package index

import (
	"fmt"
	"strconv"

	"github.com/rveen/logb"
)

// Cell is one field of one record, ready to show or export.
//
// Two representations, because a table and a CSV want different things and
// deriving one from the other loses information either way. Text is what a
// human reads: an enumeration's name, a quoted string, a hex blob. Num is the
// same value as a number where one exists, which is what a CSV column and a
// sort comparator need — and for a categorical field it is the *raw* value, not
// the label, for the same reason bucketing works on raw bits.
//
// Present is not decoration. A guarded field whose guard does not hold is not
// in the record at all (SPEC §6.2); the table shows an empty cell and the CSV
// an empty column, and neither shows a zero.
type Cell struct {
	Present bool
	Num     *float64
	Text    string
}

// Record is one decoded record of a stream.
type Record struct {
	// Axis is in the units Axis.At reports: epoch-relative ticks for a time
	// axis, the axis unit otherwise.
	Axis  float64
	Run   uint32
	Cells []Cell
}

// RecordAt materialises record i of a decoded batch against a stream's fields.
//
// It returns false when the record has no axis value, which is the one thing
// that makes a record unplaceable. Everything else — an absent field, a blob, a
// value no conversion could turn into a number — is representable and comes
// back in the cell.
//
// The batch's schema and the stream must describe the same stream. They do
// whenever the batch came from frames selected by the stream's UUID, which is
// how every caller here obtains them.
func RecordAt(b *logb.Batch, i int, st *Stream, epoch int64) (Record, bool) {
	av, err := b.Axis(i)
	if err != nil {
		return Record{}, false
	}
	rec := Record{Run: b.RunID, Cells: make([]Cell, len(st.Fields))}
	if b.Schema.AxisKind == logb.AxisTime {
		rec.Axis = float64(av.Ticks() - epoch)
	} else {
		rec.Axis = av.Float()
	}

	for k := range st.Fields {
		rec.Cells[k] = cellAt(b, i, &st.Fields[k])
	}
	return rec, true
}

// cellAt reads one field of one record.
func cellAt(b *logb.Batch, i int, fd *Field) Cell {
	// Categorical fields are read raw and labelled through the conversion, the
	// same split the charts use: equality and bucketing happen on raw bits,
	// only the display goes through the enumeration.
	if fd.Class == ClassCategorical {
		raw, err := b.Raw(i, fd.Index)
		if err != nil {
			return Cell{}
		}
		f, ok := asFloat(raw)
		if !ok {
			return Cell{Present: true, Text: fmt.Sprint(raw)}
		}
		return Cell{Present: true, Num: &f, Text: fd.Label(f)}
	}

	v, err := b.Value(i, fd.Index)
	if err != nil {
		// ErrFieldAbsent lands here, and so does a corrupt record. Both mean
		// there is no value to show; only the first is ordinary, and the scan
		// already reported the second as damage.
		return Cell{}
	}
	c := Cell{Present: true, Text: valueText(v, fd)}
	if f, ok := asFloat(v); ok && finite(f) {
		c.Num = &f
	}
	return c
}

// valueText renders a decoded value the way cmd/logbdump does, so the two tools
// agree about what a file says. Anyone comparing a chart against a dump is
// checking exactly this.
func valueText(v any, fd *Field) string {
	switch t := v.(type) {
	case []byte:
		return fmt.Sprintf("%x", t)
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		// %.6g rather than shortest-round-trip: a linear conversion off an
		// integer raw value lands on things like 40312.600000000006, and a
		// table is for reading. The exact bits are what Raw is for.
		return fmt.Sprintf("%.6g", t)
	case float32:
		return fmt.Sprintf("%.6g", float64(t))
	case uint64:
		return strconv.FormatUint(t, 10)
	case int64:
		return strconv.FormatInt(t, 10)
	case complex128:
		return fmt.Sprintf("%g%+gi", real(t), imag(t))
	}
	return fmt.Sprint(v)
}
