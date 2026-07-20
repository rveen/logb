// logbdump prints a Logb file in human-readable form.
//
//	logbdump [flags] file.logb
//
// It shows the file as what it is — a sequence of self-delimiting frames — and
// then decodes the records inside them. The structure is not decoration: a frame
// walk is how you tell a truncated file from a corrupt one, see where a segment
// restated its schemas, and find out why a stream did not decode.
//
// Two flags earn their place by demonstrating things the format claims:
//
//	-resync  enter through Resync instead of the file header, decoding a file
//	         whose beginning you do not have (SPEC.md rule 3)
//	-q       records only, for piping
//
// A file cut by power loss prints every record up to the cut and then says so.
// That is rule 2, and it is not an error.
package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"

	"github.com/rveen/logb"
)

var (
	limit    = flag.Int("n", 5, "records to print per batch; -1 for all")
	stream   = flag.String("s", "", "only this stream name")
	hexdump  = flag.Bool("x", false, "hex-dump bytes fields in full")
	quiet    = flag.Bool("q", false, "records only: no frame structure")
	resync   = flag.Bool("resync", false, "enter via Resync, ignoring the file header (rule 3)")
	showMeta = flag.Bool("meta", true, "print metadata and attachments")
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("logbdump: ")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "logbdump [flags] file.logb\n\nPrints a Logb file: frames, schemas, and decoded records.\n")
		flag.PrintDefaults()
		os.Exit(2)
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
	}

	data, err := os.ReadFile(flag.Arg(0))
	if err != nil {
		log.Fatal(err)
	}
	if err := dump(os.Stdout, data, flag.Arg(0)); err != nil {
		log.Fatal(err)
	}
}

func dump(w io.Writer, data []byte, name string) error {
	var r *logb.Reader
	var err error

	if *resync {
		// Rule 3 from the command line: throw the header away, along with a
		// chunk of the first segment, and pick the file up from the next sync
		// pattern with full schema.
		cut := len(data) / 3
		var at int
		r, at, err = logb.Resync(data[cut:])
		if err != nil {
			return fmt.Errorf("resync: %w", err)
		}
		if !*quiet {
			fmt.Fprintf(w, "%s: entered by resync, discarding the first %d bytes\n",
				name, cut+at)
			fmt.Fprintf(w, "  no file header was read; the schema below came from the segment itself\n\n")
		}
	} else {
		r, err = logb.NewReader(bytes.NewReader(data))
		if err != nil {
			return err
		}
		if !*quiet {
			fmt.Fprintf(w, "%s: %d bytes, Logb v%d.%d\n",
				name, len(data), logb.VersionMajor, logb.VersionMinor)
			fmt.Fprintf(w, "  magic % x\n\n", data[:8])
		}
	}

	// Frames arrive here as the scan passes them; batches arrive from Next. Both
	// happen inside Next, so printing from both keeps file order.
	frames := map[logb.FrameType]int{}
	if !*quiet {
		r.OnFrame = func(f logb.Frame) {
			frames[f.Type]++
			fmt.Fprintf(w, "@%-8d %-7s len=%-6d", f.Offset, f.Type, f.Len)
			if f.StreamID != 0 || f.Type == logb.FrameSchema || f.Type == logb.FrameData {
				fmt.Fprintf(w, " stream=%d", f.StreamID)
			}
			fmt.Fprintln(w)
		}
	} else {
		r.OnFrame = func(f logb.Frame) { frames[f.Type]++ }
	}

	seen := map[string]bool{}
	records, batches := 0, 0
	for {
		b, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		batches++
		records += int(b.Count)

		if *stream != "" && b.Schema.Name != *stream {
			continue
		}
		if !seen[b.Schema.Name] {
			seen[b.Schema.Name] = true
			if !*quiet {
				printSchema(w, b.Schema)
			}
		}
		printBatch(w, b)
	}

	if *showMeta && !*quiet {
		printMeta(w, r)
	}
	printSummary(w, r, frames, batches, records)
	return nil
}

func printSchema(w io.Writer, s *logb.Schema) {
	fmt.Fprintf(w, "\n  stream %q  uuid=%x\n", s.Name, s.UUID[:6])
	fmt.Fprintf(w, "    record %d bits (%d bytes), axis %s\n",
		s.RecordBits, s.RecordBytes(), axisDesc(s))
	for _, k := range sortedKeys(s.Meta) {
		fmt.Fprintf(w, "    meta %s=%s\n", k, s.Meta[k])
	}
	fmt.Fprintf(w, "    %-16s %6s %6s  %-8s %-9s %-6s %s\n",
		"FIELD", "BIT", "WIDTH", "TYPE", "ORDER", "UNIT", "CONVERSION")
	for i := range s.Fields {
		f := &s.Fields[i]
		order := "little"
		if f.BigEndian {
			order = "BIG"
		}
		bit, width := fmt.Sprint(f.BitOffset), fmt.Sprint(f.BitWidth)
		if f.Variable {
			// A variable field has no fixed bits — printing 0/0 would suggest a
			// zero-width field at offset zero rather than one that lives in the
			// tail (§6.4).
			bit, width, order = "tail", "-", "-"
		}
		fmt.Fprintf(w, "    %-16s %6s %6s  %-8v %-9s %-6s %s\n",
			f.Name, bit, width, f.Type, order, f.Unit, convDesc(f.Conv))
		if f.Guarded && int(f.GuardField) < len(s.Fields) {
			fmt.Fprintf(w, "      guard %s == %d\n",
				s.Fields[f.GuardField].Name, f.GuardValue)
		}
		for _, k := range sortedKeys(f.Meta) {
			fmt.Fprintf(w, "      meta %s=%s\n", k, f.Meta[k])
		}
	}
	fmt.Fprintln(w)
}

func printBatch(w io.Writer, b *logb.Batch) {
	n := int(b.Count)
	if *limit >= 0 && n > *limit {
		n = *limit
	}
	for i := 0; i < n; i++ {
		axis, err := b.Axis(i)
		if err != nil {
			fmt.Fprintf(w, "    [%d] axis: %v\n", i, err)
			continue
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "    %-14s %12s ", b.Schema.Name, axisStr(b.Schema, axis))
		for f := range b.Schema.Fields {
			fd := &b.Schema.Fields[f]
			if fd.Name == "t_us" { // the axis field itself; already printed as the axis
				continue
			}
			v, err := b.Value(i, f)
			if errors.Is(err, logb.ErrFieldAbsent) {
				// Not a failure: this record is a different variant. Printing
				// the error would bury the live fields in noise, and printing a
				// value would be the bug guards exist to prevent.
				continue
			}
			if err != nil {
				fmt.Fprintf(&sb, "%s=<%v> ", fd.Name, err)
				continue
			}
			fmt.Fprintf(&sb, "%s=%s ", fd.Name, valueStr(v, fd))
		}
		fmt.Fprintln(w, strings.TrimRight(sb.String(), " "))
	}
	if int(b.Count) > n {
		fmt.Fprintf(w, "    %-14s ... %d more\n", "", int(b.Count)-n)
	}
}

func valueStr(v any, f *logb.Field) string {
	switch t := v.(type) {
	case []byte:
		if *hexdump {
			return hex.EncodeToString(t)
		}
		return fmt.Sprintf("%x", t)
	case string:
		return fmt.Sprintf("%q", t)
	case float64:
		// %.6g, not shortest-round-trip: a linear conversion off an integer raw
		// value lands on things like 40312.600000000006, and a dump is for
		// reading. Anyone who needs the exact bits wants Raw, not this.
		return fmt.Sprintf("%.6g%s", t, unitSuffix(f))
	case uint64:
		return fmt.Sprintf("%d%s", t, unitSuffix(f))
	case int64:
		return fmt.Sprintf("%d%s", t, unitSuffix(f))
	}
	return fmt.Sprint(v)
}

func unitSuffix(f *logb.Field) string {
	if f.Unit == "" {
		return ""
	}
	return f.Unit
}

func axisDesc(s *logb.Schema) string {
	kind := [...]string{"time", "frequency", "angle", "distance", "index", "other"}
	k := "other"
	if int(s.AxisKind) < len(kind) {
		k = kind[s.AxisKind]
	}
	switch s.AxisMode {
	case logb.AxisImplicit:
		if s.AxisKind == logb.AxisTime {
			return fmt.Sprintf("%s, implicit, step %d ticks of 1e%d s", k, s.AxisStep.Ticks(), s.AxisExp)
		}
		return fmt.Sprintf("%s, implicit, step %g %s", k, s.AxisStep.Float(), s.AxisUnit)
	case logb.AxisExplicit:
		return fmt.Sprintf("%s, explicit in field %d, scale %d ticks of 1e%d s",
			k, s.AxisField, s.AxisScale.Ticks(), s.AxisExp)
	case logb.AxisLog:
		return fmt.Sprintf("%s, implicit log, ratio %g", k, s.AxisStep.Float())
	}
	return fmt.Sprintf("%s, axis_mode=%d", k, uint8(s.AxisMode))
}

func axisStr(s *logb.Schema, a logb.AxisVal) string {
	if s.AxisKind == logb.AxisTime {
		return fmt.Sprintf("%+.6fs", a.Seconds(s.AxisExp))
	}
	return fmt.Sprintf("%g%s", a.Float(), s.AxisUnit)
}

func convDesc(c logb.Conversion) string {
	switch t := c.(type) {
	case nil:
		return "-"
	case logb.Identity:
		return "identity"
	case logb.Linear:
		return fmt.Sprintf("linear %g + %g*x", t.A, t.B)
	case logb.Rational:
		return "rational"
	case logb.Table:
		return fmt.Sprintf("table[%d]", len(t.Keys))
	case logb.ValueToText:
		return fmt.Sprintf("enum[%d]", len(t.Keys))
	case logb.RangeToText:
		return fmt.Sprintf("ranges[%d]", len(t.Los))
	}
	return fmt.Sprintf("%T", c)
}

// sortedKeys keeps the output stable: Go randomises map iteration, and a dump
// that reorders itself between runs cannot be diffed against anything.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func printMeta(w io.Writer, r *logb.Reader) {
	if len(r.Meta) > 0 {
		fmt.Fprintln(w, "\n  metadata")
		for _, m := range r.Meta {
			fmt.Fprintf(w, "    %-14s %s\n", m.Key, m.Value)
		}
	}
	if len(r.Attachments) > 0 {
		fmt.Fprintln(w, "\n  attachments")
		for _, name := range sortedKeys(r.Attachments) {
			fmt.Fprintf(w, "    %-14s %d bytes\n", name, len(r.Attachments[name]))
		}
	}
}

func printSummary(w io.Writer, r *logb.Reader, frames map[logb.FrameType]int, batches, records int) {
	fmt.Fprintf(w, "\n  %d records in %d batches", records, batches)
	if n := frames[logb.FrameSync]; n > 0 {
		fmt.Fprintf(w, ", %d segments", n)
	}
	fmt.Fprintln(w)

	// The two things only the reader knows, and which nothing else surfaces.
	switch {
	case r.Truncated:
		fmt.Fprintf(w, "  TRUNCATED: the scan stopped at damage. Every record above is intact (rule 2).\n")
	case r.Closed:
		fmt.Fprintf(w, "  a writer closed cleanly (END frame seen)\n")
	default:
		fmt.Fprintf(w, "  input ended without an END frame — still a valid file (rule 2)\n")
	}
	for _, u := range r.Unsupported {
		fmt.Fprintf(w, "  UNSUPPORTED: %v\n", u)
	}
}
