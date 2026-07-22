package mdf

import (
	"encoding/binary"
	"fmt"

	"github.com/rveen/logb"
)

// MDF conversion types, in the standard's numbering.
const (
	ccNone uint8 = iota
	ccLinear
	ccRational
	ccAlgebraic
	ccTabInterp
	ccTab
	ccRangeToValue
	ccValueToText
	ccRangeToText
	ccTextToValue
	ccTextToText
)

var ccNames = [...]string{
	"none", "linear", "rational", "algebraic", "tab-interp", "tab",
	"range-to-value", "value-to-text", "range-to-text", "text-to-value", "text-to-text",
}

type ccData struct {
	Type       uint8
	Precision  uint8
	Flags      uint16
	RefParamNr uint16
	ValParamNr uint16
	PhyMin     float64
	PhyMax     float64
}

// Conversion is what a CC block became.
type Conversion struct {
	// Conv is the Logb conversion, or nil when this MDF conversion has no Logb
	// equivalent.
	Conv logb.Conversion

	// Kind names the MDF conversion, whether or not it survived.
	Kind string

	// Notes carries what the mapping could not express, as key/value metadata
	// for the field. Nothing is dropped in silence; some of it is just no
	// longer machine-applied.
	Notes map[string]string
}

func (c *Conversion) note(k, v string) {
	if c.Notes == nil {
		c.Notes = map[string]string{}
	}
	c.Notes[k] = v
}

// readConversion maps a CC block onto one of Logb's seven conversions (§7).
//
// It returns a nil conversion, and no error, for the MDF conversions Logb has
// no equivalent of. That is not an oversight in either format: **algebraic** is
// an embedded text formula, which SPEC.md §7 rejects on purpose — it is a
// scripting language smuggled into a data file, evaluated by a reader on bytes
// a stranger handed it. The text-keyed ones take a string as input, and a Logb
// conversion converts a number. The name comes back either way, so a caller can
// report exactly what it could not carry across instead of quietly rounding it
// off.
func readConversion(br *reader, addr int64) (*Conversion, error) {
	_, links, err := br.expect(addr, blkCC)
	if err != nil {
		return nil, err
	}
	var cc ccData
	if err := binary.Read(br.r, binary.LittleEndian, &cc); err != nil {
		return nil, err
	}
	val := make([]float64, cc.ValParamNr)
	if cc.ValParamNr > 0 {
		if err := binary.Read(br.r, binary.LittleEndian, val); err != nil {
			return nil, fmt.Errorf("conversion parameters: %w", err)
		}
	}
	out := &Conversion{Kind: "unknown"}
	if int(cc.Type) < len(ccNames) {
		out.Kind = ccNames[cc.Type]
	}

	// The reference links start after the four fixed ones.
	refs := []int64{}
	if len(links) > 4 {
		refs = links[4:]
	}
	// A reference is usually a text block. It can also be another conversion —
	// the "default" of a value-to-text table, applied to everything the table
	// does not name.
	text := func(i int) (string, error) {
		if i >= len(refs) || refs[i] == 0 {
			return "", nil
		}
		kind, err := br.kind(refs[i])
		if err != nil || (kind != blkTX && kind != blkMD) {
			return "", err
		}
		return br.text(refs[i])
	}
	defaultConv := func(i int) (*Conversion, error) {
		if i >= len(refs) || refs[i] == 0 {
			return nil, nil
		}
		kind, err := br.kind(refs[i])
		if err != nil || kind != blkCC {
			return nil, err
		}
		return readConversion(br, refs[i])
	}

	switch cc.Type {
	case ccNone:
		return out, nil

	case ccLinear:
		if len(val) >= 2 {
			out.Conv = logb.Linear{A: val[0], B: val[1]}
		}
		return out, nil

	case ccRational:
		if len(val) >= 6 {
			var p [6]float64
			copy(p[:], val)
			out.Conv = logb.Rational{P: p}
		}
		return out, nil

	case ccTabInterp, ccTab:
		keys, vals := pairs(val)
		if len(keys) == 0 {
			return out, nil
		}
		if cc.Type == ccTabInterp {
			out.Conv = logb.Table{Keys: keys, Vals: vals, Interp: true}
			return out, nil
		}
		out.Conv = nearestTable(keys, vals)
		return out, nil

	case ccValueToText, ccRangeToText:
		var keys, his []float64
		if cc.Type == ccValueToText {
			keys = val
		} else {
			keys, his = pairs(val)
		}
		texts := make([]string, len(keys))
		for i := range texts {
			if texts[i], err = text(i); err != nil {
				return nil, err
			}
		}
		def, err := text(len(keys))
		if err != nil {
			return nil, err
		}

		// The default may be another conversion rather than a text: the table
		// names a handful of sentinel values and everything else is a real
		// measurement, scaled. Logb's text conversions take a string default,
		// so the two halves cannot both survive — and it is the measurement
		// that matters. The numeric default becomes the field's conversion and
		// the named values become metadata: still in the file, still readable,
		// no longer applied.
		sub, err := defaultConv(len(keys))
		if err != nil {
			return nil, err
		}
		if sub != nil {
			out.Conv, out.Kind = sub.Conv, out.Kind+"+"+sub.Kind
			for i, t := range texts {
				out.note(fmt.Sprintf("mdf.cc.text.%g", keys[i]), t)
			}
			return out, nil
		}

		if cc.Type == ccValueToText {
			out.Conv = logb.ValueToText{Keys: keys, Texts: texts, Default: def}
		} else {
			out.Conv = logb.RangeToText{Los: keys, His: his, Texts: texts, Default: def}
		}
		return out, nil
	}
	return out, nil
}

// nearestTable expresses MDF's nearest-key lookup as a Logb table.
//
// This is the one conversion whose rule genuinely differs between the two
// formats: without interpolation MDF picks the key *nearest* the raw value,
// while Logb's table picks the nearest at or below it. Moving each key to the
// midpoint of the gap before it makes Logb's rule compute MDF's — everywhere,
// not just on the keys themselves. A table that was only right where it was
// sampled would be worse than no conversion, because it would look right.
func nearestTable(keys, vals []float64) logb.Table {
	mid := make([]float64, len(keys))
	copy(mid, keys)
	for i := 1; i < len(keys); i++ {
		mid[i] = (keys[i-1] + keys[i]) / 2
	}
	return logb.Table{Keys: mid, Vals: vals}
}

// pairs splits interleaved (key, value) parameters into two slices.
func pairs(v []float64) (a, b []float64) {
	n := len(v) / 2
	a, b = make([]float64, n), make([]float64, n)
	for i := 0; i < n; i++ {
		a[i], b[i] = v[2*i], v[2*i+1]
	}
	return a, b
}
