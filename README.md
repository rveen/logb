# logb

[![Go Reference](https://pkg.go.dev/badge/github.com/rveen/logb.svg)](https://pkg.go.dev/github.com/rveen/logb)

A self-describing binary format for time-series measurement, bus-trace, and
simulation recording — designed to be written by embedded loggers and read by
analysis tools. This repository holds the [draft spec](SPEC.md) (v0.1) and a
reference implementation in Go.

*Logb* is `log` plus `b` for binary — the convention that makes `.xlsb` the
binary twin of `.xlsx` and `jsonb` the binary form of JSON. File extension
`.logb`.

## Why another format?

**The automotive industry has no open standard for test data.** The one in common
use is [MDF](https://www.asam.net/standards/detail/mdf/) — ASAM's Measurement Data
Format — and it is considerably better than nothing, but it is not open: the
specification sits behind a paywall. That makes the price of writing a conforming
reader a licence, and the price of *checking* someone else's reader another one. A
format whose definition you cannot read is not a standard you can build on; it is
a vendor relationship. The main alternative in CAN logging, Vector's BLF, is
proprietary outright.

That is the gap this fills. [SPEC.md](SPEC.md) is in this repository under the
same terms as the code, it is complete enough to implement from, and a conforming
reader is about a thousand lines plus a decompressor. There is nothing to buy and
nobody to ask.

It follows that **everything Logb knows about MDF comes from publicly available
information** — open-source implementations, tool behaviour, published material,
and what practitioners have written down. That is deliberate, and it also bounds
the criticism: where this project says MDF4 leaves unaligned big-endian undefined
([CAN.md](CAN.md)), or embeds a formula language in a conversion, or carries
fifteen data types including a "canopen date", those are claims about what is
publicly known, not a reading of the paywalled text. Learning from a format's
known mistakes is most of what this design is.

The other parent is the **SPICE `.raw` file** that ngspice and LTspice write. It
gets the important things right: raw values, a self-describing header, no external
schema, and simple enough that every EDA tool reads it. It also demonstrates the
cost of the one thing it gets wrong — `No. Points:` is stated up front, so the
file cannot be written until the run has finished. SPEC.md §11 is a complete
mapping from `.raw` onto Logb, and under Logb a simulator emits frames as it
solves, so a long transient analysis becomes watchable while it runs. That falls
out of "nothing points forward" without being designed for.

Logb is not an automotive format. It was born in that context, but the domain axis
is general rather than time — a transient sweeps time, an AC analysis sweeps
frequency, a sweep can be angle or distance — so bus traces, bench measurements,
and simulator output are the same kind of file here.

## Isn't this a solved problem?

It is a fair challenge, and it deserves a real answer rather than a dismissal.
The honest finding is that the problem is *half* solved, twice, by two families of
format that do not overlap — and a bus or bench recording needs both halves.

**General-purpose containers solve the file and punt on the payload.**
[MCAP](https://mcap.dev/spec) is the closest thing to Logb that exists, and it is
genuinely good: row-oriented, append-only, chunked, zstd/lz4, attachments,
metadata, an optional summary. If you are logging ROS or protobuf messages, stop
reading and use it. But its messages are, in the spec's own words, *"opaque bytes
to be decoded according to the schema of the channel"*, and its
[schema registry](https://mcap.dev/spec/registry) lists protobuf, flatbuffer,
ros1msg, ros2msg, ros2idl, omgidl, and jsonschema — **nothing for CAN, DBC, or
bit-level extraction**. Record a CAN bus into MCAP and you have stored the frame;
naming `EngineSpeed` still needs the DBC sidecar.

Opacity itself is not the complaint, and it is worth being exact about that,
because [SPEC.md §6.9](SPEC.md) makes the same call MCAP does for genuinely
serialised payloads. **The complaint is opacity about data that is describable.**
A CAN signal is a bit slice at a fixed offset, so an offset-based schema reaches
it, and a format that stores it as an undecoded blob has declined to describe
something it could have. A SOME/IP payload member sits at an offset determined by
the values before it, so no offset-based schema reaches it, and Logb keeps the
bytes and says so. The difference is visible in the header: Logb describes a
SOME/IP message's sixteen fixed bytes as fields, and a schema-registry format has
no way to.

[Avro](https://avro.apache.org/docs/1.11.1/specification/) falls short somewhere
else again: its sync-marker design prefigures §4, but its schema lives in the
header, *once*, so a reader handed the middle of a file has framing and no
meaning.

**Measurement formats solve the payload and are closed, or fall short.** MDF4 has
a real bit-level signal model and is paywalled, with unaligned big-endian left
undefined ([CAN.md](CAN.md)). NI's
[TDMS](https://www.ni.com/en/support/documentation/supplemental/07/tdms-file-format-internal-structure.html)
is the near-miss that makes the point best: it is openly documented, streaming,
and segment-structured, which is Logb's shape exactly. Then its metadata turns out
to be *incremental* — written into a segment "only if it changes" — so a cut file
loses everything stated earlier; its byte order is a per-segment ToC flag rather
than per field, so a frame mixing Intel and Motorola signals is inexpressible; it
has no bit-level fields; and it wants defragmentation and a `.tdms_index` sidecar.

**HDF5 is the "just use a real scientific container" answer, and it is a
filesystem inside a file.** The
[format spec](https://support.hdfgroup.org/documentation/hdf5/latest/_f_m_t3.html)
carries a superblock — found by searching byte offsets 0, 512, 1024, 2048, "and so
on" — plus object headers, local and global heaps, a free-space manager, v1 *and*
v2 B-trees, and five distinct index types for dataset chunks. Rule 4 says a
conforming reader is about a thousand lines and a decompressor; an HDF5 reader is
not within an order of magnitude of that, which is why in practice there is one
implementation and everything else binds to it.

The complexity and the fragility are the same fact. A B-tree is a mutable pointer
structure that must be patched in place as it grows — precisely what rule 1
forbids, and precisely why the forum threads are full of `truncated file: eof =
…, stored_eof = …`, `h5clear`, and SWMR. Logb's index (§9) is the same idea with
the authority removed: it is written once at the end, it is rebuildable by scan,
and a reader that disagrees with it treats *the index* as the corrupt part. An
index that cannot lie is an index that cannot take the file down with it.

So the gap is not "another container" or "another signal model". It is that **no
open format has both**, and the two halves cannot simply be stacked: putting a
bit-level schema *inside* a container is what makes a cut file decodable, and
that is a decision about the container, not a payload you can bolt on.

| | Payload model | A file cut mid-write | Schema lives | Open |
|---|---|---|---|---|
| **Parquet / Arrow** | columnar, typed | unreadable — footer required | footer | yes |
| **Avro** | row, typed, byte-aligned | framing survives, meaning does not | header, once | yes |
| **MCAP** | opaque bytes + external schema | readable after `mcap recover` | before first use | yes |
| **HDF5** | typed arrays, B-tree indexed | `h5clear`, SWMR, or lost | superblock + heaps | yes |
| **TDMS** | typed channels, no bit fields | loses earlier metadata | incremental | documented |
| **MDF4** | bit-level signals | links point forward | linked blocks | **paywalled** |
| **BLF** | CAN frames | — | — | **proprietary** |
| **DBC** | bit-level signals | it is a text sidecar, not a container | | de facto |
| **Logb** | bit-level fields, per-field order, raw + conversion | is a valid file (rule 2) | every segment | yes |

Two entries are worth dwelling on, because they are the honest ones. Parquet's
[docs](https://parquet.apache.org/docs/file-format/) say *"file metadata is
written after the data to allow for single pass writing"* and *"readers are
expected to first read the file metadata"* — a fine trade for analytics, fatal for
a logger, and the reason Parquet is a **downstream target** for Logb rather than a
rival. And MCAP ships a `mcap recover` subcommand that "reads a potentially corrupt
or truncated MCAP file and writes a valid, readable copy". That tool is not a flaw;
it is a reasonable answer. It is just a different answer from rule 2, which says a
truncated file **is** a valid file and there is nothing to run.

Where the critics are right, and this should be said plainly: Logb has no
ecosystem. MDF4 and BLF open in every automotive tool on the market and this opens
in nothing but the reader in this repo. That is a real cost, paid deliberately
against a spec you can read for free and a bit rule that has one defined answer at
every offset.

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
Motorola signals, so byte order is per-field, not per-file — and bit numbering
follows the byte order, which is what makes the Motorola sawtooth disappear and
reduces a DBC importer to a one-line offset transform. That story is
[CAN.md](CAN.md).

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
- [CAN.md](CAN.md) — what Logb fixes about DBC/MDF4 bit ordering, with diagrams
- [GNSS.md](GNSS.md) — storing GNSS: scaled integers, the two clocks, and raw observables
- [STATUS.md](STATUS.md) — implementation state and the design decisions
- [pkg.go.dev](https://pkg.go.dev/github.com/rveen/logb) — API reference

## License

[MIT](LICENSE), covering the specification as well as the code — which is the
point of the section above. Implement it, fork it, sell it; there is nothing to
buy and nobody to ask.
