# logb

[![Go Reference](https://pkg.go.dev/badge/github.com/rveen/logb.svg)](https://pkg.go.dev/github.com/rveen/logb)

A self-describing binary format for time-series measurement, bus-trace, and
simulation recording — designed to be written by embedded loggers and read by
analysis tools. This repository holds the [draft spec](SPEC.md) (v0.1) and a
reference implementation in Go.

*Logb* is `log` plus `b` for binary — the convention that makes `.xlsb` the
binary twin of `.xlsx` and `jsonb` the binary form of JSON. File extension
`.logb`.

## What problem it solves

Most logging formats assume the writer gets to finish. They put a directory at
the end, or state the schema once at the front, or link blocks together with
offsets that must all be patched before the file means anything. Pull the power
mid-write and you have a file that a repair tool might partially rescue.

Logb is designed as a **varve**: one season's sediment couplet in a lake bed,
laid down in sequence, never rewritten, each layer self-dating, readable from any
cut face of the core — including a core that snapped. Concretely:

- **Nothing points forward.** A frame may reference earlier bytes, never later
  ones. The writer never seeks back to patch a field it already emitted, so it
  needs only an `io.Writer`.
- **Append-only and crash-safe.** A file truncated at an arbitrary byte is a
  valid file containing every record up to the last intact frame. No repair tool,
  no recovery mode.
- **Cut anywhere, decode.** Hand a reader the middle of a file, with no access to
  the start, and it resynchronises and decodes records with full schema. Schema
  is restated per segment, not stated once.
- **No dependencies.** A conforming reader is implementable in ~1000 lines with
  only a decompressor. No XML, no external schema registry, no library that must
  still exist in 2050.
- **Raw is preserved.** Stored values are the bits the sensor or bus produced;
  physical values are derived by a declared conversion, so a read-modify-write
  round trip is byte-identical.
- **Fixed cost per record.** Adding a channel does not change the cost of
  decoding an unrelated channel.

The bit layout lives *in the file*, not in a sidecar the reader must go find a
parser for. A CAN recording carries its signal definitions, so a tool with no DBC
can still name and scale every signal.

Everything unknown is skipped at a defined blast radius rather than guessed at:
an unknown frame type costs nothing, an unknown codec or filter costs that frame,
an unknown axis mode costs that stream. That is what makes a future codec or axis
mode safe to add — a v0.1 reader decodes a v0.9 file's v0.1 streams and reports
the rest as unsupported instead of returning garbage shaped like data.

## Status

**Draft, for discussion.** The format is v0.1 and not frozen; see
[STATUS.md](STATUS.md) for what is implemented, what is not, and the design
decisions behind it. `testdata/can-example.logb` is a byte-reproducible
conformance artifact rather than a convenience — if regenerating it changes the
bytes, the format changed.

## Install

```sh
go get github.com/rveen/logb
```

## Concepts

A file is a sequence of **segments**, each a self-contained decode unit that
restates every schema its data frames need. Within a segment, **DATA frames**
carry batches of fixed-size records; a **schema** declares the record's bit
layout, its axis, and its identity.

The **axis** is general rather than time-specific: a transient sweeps time, an AC
analysis sweeps frequency, MDF4 masters can be angle or distance. It is either
*implicit* (`base + i*step`, zero bytes per record), *explicit* (read from a
field), or *log*-spaced (`base * ratio^i`, which is what an AC decade sweep
actually is).

Each field declares its bit offset, width, type, byte order, and a
**conversion** from raw to physical (`Identity`, `Linear`, `Rational`, `Table`,
`ValueToText`, `RangeToText`). A single CAN frame routinely mixes Intel and
Motorola signals, so byte order is per-field, not per-file.

## Example

Writing a stream and reading it back. The full worked example — multiple
segments, mixed byte orders, variable-length fields, compression — is in
[`internal/example`](internal/example/example.go).

```go
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/google/uuid"
	"github.com/rveen/logb"
)

// A schema declares the record layout, the axis, and the stream's identity.
// The UUID says which logical stream this is across segments and files: persist
// it across file rollover so the files concatenate into one recording, and
// generate a fresh one per instrument so two identical loggers do not merge.
func schema() *logb.Schema {
	return &logb.Schema{
		UUID:       uuid.NewSHA1(uuid.NameSpaceOID, []byte("example/EngineData")),
		Name:       "EngineData",
		RecordBits: 24, // EngineSpeed(16) + CoolantTemp(8)

		// One record every 10 ms, derived from the record index: the axis costs
		// zero bytes per record. AxisExp is the tick size as a power of ten, so
		// -9 means one tick is a nanosecond.
		AxisKind: logb.AxisTime,
		AxisMode: logb.AxisImplicit,
		AxisExp:  -9,
		AxisUnit: "s",
		AxisStep: logb.TickVal(10_000_000),

		// Raw is what the bus produced; Conv derives the physical value.
		Fields: []logb.Field{
			{Name: "EngineSpeed", BitOffset: 0, BitWidth: 16, Type: logb.TypeUint,
				Unit: "rpm", Conv: logb.Linear{A: 0, B: 0.25}},
			{Name: "CoolantTemp", BitOffset: 16, BitWidth: 8, Type: logb.TypeUint,
				Unit: "degC", Conv: logb.Linear{A: -40, B: 1}},
		},
	}
}

// The writer needs only an io.Writer — it never seeks back.
func write(w io.Writer) error {
	s := schema()
	if err := s.Validate(); err != nil {
		return err
	}

	lw, err := logb.NewWriter(w)
	if err != nil {
		return err
	}
	if err := lw.AddStream(s); err != nil {
		return err
	}

	// A segment restates its schemas, which is what lets a reader start here.
	// Start a new one every N megabytes or M seconds — that is writer policy,
	// not a spec requirement.
	if err := lw.BeginSegment(1773480413000000000); err != nil { // wall clock, ns
		return err
	}
	if err := lw.WriteMeta("vehicle.vin", "WVWZZZ1JZXW000001"); err != nil {
		return err
	}

	// Records are packed to the schema's bit layout: 100 of them, back to back.
	const n = 100
	rec := make([]byte, 0, n*s.RecordBytes())
	for i := 0; i < n; i++ {
		rec = binary.LittleEndian.AppendUint16(rec, uint16(3200+i*4)) // rising rpm
		rec = append(rec, byte(80+i/10))                              // warming up
	}
	if err := lw.WriteData(s, logb.TickVal(0), 0, n, rec); err != nil {
		return err
	}

	// Close writes the index and end frames. Both are optional by construction:
	// a file that lost power simply lacks them and still reads.
	return lw.Close()
}

// The reader needs only an io.Reader, and yields one batch per DATA frame.
func read(data []byte) error {
	r, err := logb.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	for {
		b, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		for i := 0; i < int(b.Count) && i < 3; i++ {
			axis, err := b.Axis(i) // b.Raw(i, f) gives the stored bits instead
			if err != nil {
				return err
			}
			rpm, err := b.Value(i, 0) // conversion applied
			if err != nil {
				return err
			}
			degC, err := b.Value(i, 1)
			if err != nil {
				return err
			}
			fmt.Printf("%s t=%.3fs EngineSpeed=%v rpm CoolantTemp=%v degC\n",
				b.Schema.Name, float64(int64(axis))*1e-9, rpm, degC)
		}
	}

	// Truncated means the scan stopped at damage rather than at a clean end.
	// Every batch returned before it was set is intact and trustworthy.
	if r.Truncated {
		fmt.Println("file is truncated; batches above are still intact")
	}
	fmt.Println("meta:", r.Meta)
	return nil
}

func main() {
	var buf bytes.Buffer
	if err := write(&buf); err != nil {
		panic(err)
	}
	fmt.Printf("wrote %d bytes\n", buf.Len())
	if err := read(buf.Bytes()); err != nil {
		panic(err)
	}
}
```

Output:

```
wrote 708 bytes
EngineData t=0.000s EngineSpeed=800 rpm CoolantTemp=40 degC
EngineData t=0.010s EngineSpeed=801 rpm CoolantTemp=40 degC
EngineData t=0.020s EngineSpeed=802 rpm CoolantTemp=40 degC
meta: [{vehicle.vin WVWZZZ1JZXW000001}]
```

### Recovering a cut file

`logb.Resync` is rule 3 as an API: hand it a byte slice starting anywhere, and it
scans forward to the next sync frame and returns a reader positioned there, along
with the offset it entered at. Records before the cut are gone; everything after
it decodes with full schema.

```go
r, off, err := logb.Resync(data[7000:])
```

## Tools

```sh
go run ./cmd/logbgen -o /tmp/x.logb       # write the example file
go run ./cmd/logbdump /tmp/x.logb         # frames, schemas, records
go run ./cmd/logbdump -resync /tmp/x.logb # resynchronise, from the CLI
```

That second one is the quickest way to see the crash-safety claim hold up:

```sh
head -c 7000 /tmp/x.logb > /tmp/cut.logb
go run ./cmd/logbdump /tmp/cut.logb       # 302 records, TRUNCATED
```

## Documentation

- [SPEC.md](SPEC.md) — the format, 12 sections, with conformance vectors
- [STATUS.md](STATUS.md) — implementation state and the design decisions
- [pkg.go.dev](https://pkg.go.dev/github.com/rveen/logb) — API reference
