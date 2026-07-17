# Logb — status, 2026-07-17

Handoff notes. Where the work is, what's decided, what's open, what to do next.

## What this is

A clean-slate open standard for measurement/bus/simulation logging, meant to
replace what MDF4 (ASAM, automotive) and the SPICE `.raw` format do, without
inheriting their mistakes. `SPEC.md` is the draft spec; the Go files next to it
are a working reference implementation.

Three framing decisions, already made, that everything else follows from:

1. **Logger-first**, analysis-second. Optimised for embedded writers: append-only,
   crash-safe, low RAM. Analysis tools index or convert.
2. **No MDF4 compatibility constraint.** Take the good ideas, drop the rest. No
   migration path is owed to existing MDF4 tooling.
3. **Self-contained binary spec.** No CBOR, no Arrow, no XML. A conforming reader
   is implementable with no dependencies beyond a decompressor, because the spec
   must outlive its libraries. The reference implementation now takes
   `klauspost/compress` for zstd — rule 4 always conceded that one dependency,
   and a reader that sticks to `none`/`deflate` still needs nothing but stdlib.

**The load-bearing idea: sequence, not graph.** MDF4's original sin is that it is
a pointer graph — blocks reference each other by absolute file offset, so a writer
must seek back and patch links after the fact. That is why MDF4 readers need
`io.ReadSeeker`, why "unsorted records" exist, and why a logger that loses power
leaves links pointing into the void. Logb frames are self-delimiting, CRC'd, and
never point forward. A writer only appends; a reader only scans.

## State: working, tested, uncommitted

```
github.com/rveen/logb/
  SPEC.md        draft spec v0.1, 12 sections
  STATUS.md      this file
  logb.go        types, constants, AxisVal, Schema, Schema.Validate, CRC
  convert.go     conversions + bit extraction
  wire.go        LE encode/decode helpers, transpose filter
  writer.go      Writer — needs only io.Writer; enforces run contiguity
  reader.go      Reader — needs only io.Reader; Resync(); OnFrame trace hook
  logb_test.go   23 tests
  example_test.go              8 tests against the fixture (package logb_test)
  testdata/can-example.logb    15 KB, generated, golden
  internal/example/            the generator, shared by the tool and the test
  cmd/logbgen/                 writes the example file
  cmd/logbdump/                pretty printer
```

31 tests, all passing (37 counting subtests).

```sh
go run ./cmd/logbgen -o /tmp/x.logb       # same bytes every run
go run ./cmd/logbdump /tmp/x.logb         # frames, schemas, records
go run ./cmd/logbdump -resync /tmp/x.logb # rule 3, from the CLI
head -c 7000 /tmp/x.logb > /tmp/cut.logb
go run ./cmd/logbdump /tmp/cut.logb       # rule 2: 302 records, TRUNCATED
```

`testdata/can-example.logb` is a conformance artifact rather than a convenience.
It is byte-reproducible (`TestExampleDeterministic`) and `TestExampleGolden` fails
if the format's output moves at all; `-update` is how you say a move was meant.
The invented CAN traffic is chosen to hit what is easy to get wrong: a raw wire
stream with an 8-byte `bytes` payload, two decoded streams mixing Intel and
Motorola signals **in the same frame** (including a 24-bit Motorola odometer that
is unaligned and crosses byte boundaries — the case §6.2 exists for), an events
stream whose message is the file's only §6.4 tail, three segments, and one segment
written transposed and deflated.

`Schema.Validate` is where the combinations the spec leaves undefined get caught
before they reach a file: an unknown axis mode, log-on-time, an unusable ratio.
`AddStream` calls it, so a writer cannot produce one by accident.

`Reader.Unsupported` is the other side of that: a schema with an axis mode this
version does not define, or a DATA frame with a codec or filter it does not
implement, is **skipped and recorded, not treated as damage**. `Truncated` stays
false — a file from a later version is not a broken file — and every other stream
in it reads normally. Three levels of the same rule, and it is what makes a future
axis mode, codec, or filter safe to add:

| Unknown thing | Fatal to | Reader does |
|---|---|---|
| frame type | nothing | skips by length (§3.3) |
| `codec` / `filter` | the frame | skips it, records in `Unsupported` (§8) |
| `axis_mode` | the stream | leaves the id unbound, records it (§5) |

Verify with (from this directory, the module root):

```sh
go test ./... -count=1
go vet ./... && gofmt -l .
```

**Not yet committed — `git status` shows the whole tree untracked.** Commit before
switching machines; nothing here exists anywhere else.

## Decisions made (do not re-litigate without a reason)

| Decision | Why |
|---|---|
| Frames, never pointers | Crash safety and `io.Reader`-only writers fall out of it |
| Schema restated per segment | Lets a reader decode a file cut anywhere; MDF4 cannot do this at all |
| Axis is general, not time | AC sweeps frequency, DC sweeps a source, MDF4 masters can be angle/distance. Hardcoding time was a bug I made and fixed |
| `axis_exp` decimal tick | Femtosecond simulation stays integer-exact. Same device as VCD's `$timescale` |
| `complex` is a first-class type | AC analysis: re/im are one quantity sharing name, unit, conversion |
| Runs are explicit (`run_id` + RUN frame) | LTspice makes you guess `.step` boundaries; see below |
| `stream_id` u16 segment-scoped + `stream_uuid` in schema | Splits routing from identity; makes concatenation byte-concatenation |
| Index has no authority | Rebuildable by scan; a disagreeing index is a corrupt index, not corrupt data |
| END frame is informational | A statement about the past, not a command to stop |
| `identity` conversion is type-preserving | Otherwise every bool channel reads back as float64 |
| No algebraic conversion | MDF4 embeds a text formula; it is a scripting language smuggled into a data format and an RCE surface |
| Unsorted records dropped | They exist because writers couldn't buffer per group. Buffer per group |
| Log-spaced implicit axis (`axis_mode=2`) | An AC decade sweep is uniform in log space and nowhere else; zero bytes per record. Cheap before 0.1, breaking after |
| Runs contiguous within a segment | Per-frame `run_id` stays, but shuffled runs are forbidden: otherwise a streaming reader must hold every run open until end of segment |
| Signing reserved (`0x50`), not specified | A detached signature is void after truncation or concatenation — the two things this format promises to survive. Per-segment is the only shape that fits |
| Encryption permanently out of scope | Open format; confidentiality belongs to the layer that stores or moves the file |
| No compression dictionaries | Shared across segments contradicts what a segment is; restated per segment is not sharing. A dictionary by id would break rule 4. Bigger batches are the cheaper answer, and a later version can add one as a codec id at no cost |
| Unknown axis_mode / codec / filter are rejected, never defaulted | The property that makes each of them extensible later. A reader that guesses returns garbage shaped like data |
| Bit numbering follows the byte order | Each order's fields are contiguous in its own numbering, so the rule has no alignment case and the Motorola sawtooth never appears. BE is exactly DBC |
| Conformance vectors are normative (§6.2) | Every prose statement of bit numbering ever written has been ambiguous or wrong, including two in this repo's own history. Vectors do not have that failure mode |

### The motivating example, worth keeping

`/files/go/src/github.com/rveen/ltspice/lta/main.go:79` and `:219` recover
`.step` run boundaries by guessing:

```go
// detect LT runs (time == 0)
if i > 0 && m[0][i] == 0 {
```

That heuristic is wrong for any run whose axis legitimately revisits zero — a DC
sweep from -5 V to +5 V, an AC sweep including DC, a transient with a negative
start. The information exists at write time and LTspice throws it away. §6.5
exists to make that heuristic unnecessary, and `TestSteppedRuns` reproduces
exactly the case that defeats it.

## Bugs the implementation found in the spec

Writing the code was worth it specifically because of these. All are fixed in
`SPEC.md`. The pattern is worth naming: **every one was found by building
something, and none by re-reading the prose.** Two were found by planning the
example file, before a line of it was written; two more by writing it.

1. **Conversions assumed every field is numeric.** A `bool` read back as
   `float64`, because a nil conversion encodes as `identity` and the reader piped
   everything through `raw → float64 → convert`. Fix: identity is type-preserving;
   the pipeline is entered only when a conversion is actually present. MDF4 has
   this same disease.
2. **The concatenation claim in §6.6 was false.** File A's END frame stopped the
   reader dead, and file B's index offsets were wrong once B moved. Fixes: END is
   informational (a joined file has END frames in the middle, correctly); index
   offsets are self-relative, counted backwards from the INDEX frame's own start.
3. **Big-endian bit numbering was undefined**, and the draft's proposed fix was
   worse than undefined: it was wrong. See #9 below — the spec asserted that
   forbidding unaligned BE would cost DBC importers "a shift", which is false, and
   an executable check is what exposed it. Prose about bit numbering should be
   assumed wrong until a vector says otherwise.
4. **§6.3 said `bytes`/`string` were "variable, tail"; §6.4 said bus payloads
   should be "fixed-width `bytes` fields".** Both cannot be true. The fixed form
   is the common one — a CAN payload — and the table simply omitted it.
5. **§6.4 and §8 disagreed about where tails go.** §6.4 read as though each
   record were followed by its own tail; §8 said "record_count records, then
   tails". Only §8 can be right, because transpose needs the fixed portions
   contiguous and equally sized. §6.4 now states the layout and says why.
6. **A variable field's `bit_offset`/`bit_width` were undefined** — now pinned to
   `bit_width = 0`, contributing nothing to `record_bits`.
7. **§7 forbade conversions on `bytes`/`string` but nothing enforced it.** The
   reader would have routed such a field through `toFloat`, failed, and handed
   back the raw value as though no conversion had been requested — silently
   producing something that looks converted. Now `Schema.Validate` rejects it.

Two implementation bugs of the same family, found by the same exercise:

- **Transpose ran over the tails.** Both writer and reader applied the filter to
  the whole payload, though §8 says it covers the fixed portion only. It was
  invisible because `transpose` declines any input that is not a whole number of
  records, and fixed+tails almost never is — so the filter silently did nothing
  rather than corrupting anything. It would have started corrupting the day a
  tail happened to make the length divide evenly.
- **`buf.kv` iterated a Go map**, so schema metadata reached the wire in a random
  order and the same input produced different bytes on every run. No golden
  fixture, no content hash, no diffing two recordings. Keys are now sorted.

## Open questions (`SPEC.md` §12)

Resolved: #2 (stream identity), #4 (sub-nanosecond time), #3 (encryption out of
scope, signing reserved as frame `0x50`), #5 (log axis is `axis_mode=2`), #6 (runs
contiguous within a segment), #7 (dictionary sharing dropped), #8 (`record_bits`
is a `u32`), and #9 (bit numbering follows the byte order).

**Only #1 remains: the name.** Everything else in §12 is closed.

**#9 is settled: bit numbering follows the byte order.** `bit_offset` names the
field's first bit — from the LSB of its byte for little-endian, from the MSB for
big-endian — and the field runs upward. No alignment case, no sawtooth, and the
big-endian half is exactly DBC Motorola, so a DBC import is one line of offset
arithmetic (`8*(start/8) + (7 - start%8)`) and no data movement.

Two things are worth knowing about how that was reached, because both change what
the next person should trust:

- **The old answer was wrong on its facts, not just suboptimal.** Forbidding
  unaligned BE and telling importers to normalise "at the cost of a shift" does
  not work: a Motorola signal has *no* little-endian equivalent unless it fits in
  a single byte. Only 288 of the 1,552 Motorola signals in an 8-byte frame have
  one, and none is wider than a byte — the ordinary aligned 16-bit signal included.
  Mandating one byte order does not move work to importers, it deletes the data.
- **It was settled with an executable oracle, not with DBC files.** `TestDBCMotorola`
  runs Vector's reference algorithm against `extractBits` over 465,600 cases of
  position, width, and payload: zero disagreements. That is a stronger check than
  a corpus of real DBCs would have been, and it does not rot.

`ErrUnalignedBE` is gone, along with `Validate`'s rejection of unaligned BE: there
is nothing left to reject.

**#1 is settled: the format is Logb**, extension `.logb`, magic
`89 4C 4F 47 42 0D 0A 1A`, sync token `LOGBSYNC` + the same eight random bytes.
`log` plus `b` for binary, as `.xlsb` is to `.xlsx`. The varve — the sediment
couplet geologists count to date events — stays as the design rationale in §1 of
the spec, which is what it was always doing; it is no longer what the name says.

The reasoning is in §12.1 and is worth not re-deriving: **every name collision hit
during this search came from a name that already meant something to someone.**
`BLF` is Vector's proprietary CAN log — the thing this format most directly
replaces. `AXL` is ETAS's, also automotive. `.ax` is a Windows DirectShow filter,
i.e. executable code. `.vv`, the first choice of extension, is virt-viewer's
config format and would fight for the desktop handler on the Linux boxes where
analysis tools run. Cadence would have been a fine name for a format built on
periodic sampling, and is an EDA giant. `.dlog` is Keysight's power-analyser data
log — binary, instrument-written, this exact domain.

The three-letter space is exhausted and the automotive part of it is owned by the
incumbents. `.logb` does not compete for it: it is descriptive rather than coined,
parses on sight, and is unclaimed. Known costs, accepted: `+b` implies a text twin
that does not exist, and `logb` echoes Go's `math.Logb` (no import conflict — both
are package-qualified — but it owns the search results).

**With this, every question in `SPEC.md` §12 is closed.** The draft is nameable,
freezable, and — still — uncommitted.

## Next steps

1. **Commit the tree.** It is untracked.
2. **Settle open question 9** against real DBC files.
3. **Write the ngspice importer** — `SPEC.md` §11 is a complete mapping table from
   SPICE `.raw` to Logb. The existing parser at
   `/files/go/src/github.com/rveen/ltspice/ltspice.go` reads LTspice IV (ASCII
   header) and XVII (UTF-16LE header) and is the place to start. Known quirks,
   all importer problems rather than format problems: the axis variable is always
   `f64` even when other variables are `f32`; LTspice stores a marker in the time
   axis's sign bit, requiring `abs()`; `Flags: compressed` is LTspice's own scheme
   and unsupported by that parser.
4. **Consider an MDF4 importer** — `/files/go/src/golib/formats/mdf/mdf.go` is a working MDF4 reader
   (1200 lines) and the conversion is lossless in principle, even though no
   compatibility is owed.

## Things not yet implemented

- The INDEX frame is written and skipped on read. No seek API exists; the reader
  is single-pass by design and the index is a pure accelerator.
- Codec lz4 is specified but unimplemented. zstd (the default) and deflate
  both work; an lz4 frame is rejected and recorded in `Unsupported`, which is
  §8's defined behaviour rather than a gap in it.
- `cmd/logbdump` has no golden test of its own output. The fixture pins the
  bytes; nothing pins the rendering.
