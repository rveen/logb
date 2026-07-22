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
  spice/raw.go                 SPICE raw reader (LTspice IV ASCII + XVII UTF-16)
  spice/convert.go             SPICE raw → Logb, the SPEC §11 mapping
  spice/spice_test.go          6 tests, against testdata/test{,.op}.raw
  cmd/raw2logb/                the importer as a command
  mdf/block.go                 MDF4 block layer: DG/CG/CN/CC/AT, DT/DL/DZ/HL
  mdf/mdf.go                   the model: groups, channels, records, VLSD
  mdf/sample.go                decoding a channel by MDF's own bit rules
  mdf/conv.go                  MDF conversions → the seven in §7
  mdf/convert.go               MDF4 → Logb
  mdf/{mdf,convert}_test.go    16 tests, against testdata/mdf/*.mf4
  mdf/bus.go                   CAN bus recording → decoded signal streams
  cmd/mdf2logb/                the importer as a command
  dbc/dbc.go                   Vector DBC parser
  dbc/schema.go                DBC message → Logb schema; multiplexing → guards
  dbc/dbc_test.go              8 tests
  testdata/obd2.dbc            an OBD2 database for the CAN fixture
  internal/tick/               axis tick sizing, shared by both importers
```

The SPICE importer is §11 executed rather than asserted. A transient's time axis
becomes `axis_kind=time`, `axis_mode=explicit` over an **i64 tick** field (seconds
in a float field would be truncated by `AxisAt`'s `int64(explicit)`), with
`axis_exp` chosen as the finest of −15…−3 that keeps the run exact through the
`float64` that `Batch.Axis` routes the field through. Values are copied verbatim —
the raw file already stores little-endian f32/f64 in schema order — so only the
axis is rewritten. LTspice's sign-bit-on-time marker is normalised away with
`abs()` (the fixture has 18 such points, and is monotonic once they are absolute),
an operating point becomes `axis_kind=index`, and `compressed`/`fastaccess` are
refused rather than misread. `.step` boundaries are recovered once, at import,
from the axis restarting, and written as RUN frames — the heuristic below, run
exactly once and never again by a reader.

The MDF4 importer is the same exercise against the format Logb is a reaction to,
and it is the strongest evidence the design has. **The record is copied
verbatim** and the axis prepended, so every field keeps the bit offset MDF gave
it — which works only because §6.3's bit numbering *is* MDF's, little-endian
fields from the low bit up and big-endian ones from the high bit down. The test
converts all five fixtures and compares every field of every record against what
this repository's own MDF decoder — written from the standard, not from
`logb/convert.go` — makes of the original. 100 000 records of one file, 1 619 CAN
frames of another, both framings including `filter=transpose`: all identical. A
one-bit error in the offset calculation fails it immediately, which was checked
by making one.

What the mapping shows about the two formats:

- **Unsorted data groups** (§10) are demultiplexed once, at import. MDF
  interleaves several groups' records and tags each with an id, and every reader
  pays for that on every read, forever.
- **Invalidation bits** become guarded fields (§6.2) with no bytes added: the
  bits are already in the record, and "the sample is not valid" is what an absent
  field means.
- **Variable-length signal data** — how MDF stores a CAN payload — is resolved
  and inlined as a fixed-width `bytes` field, which is what §6.4 says a bus
  payload should be. The batch keeps its seekability; the indirection was buying
  nothing.
- **Composed channels** are flattened, so `CAN_DataFrame.ID` is a 29-bit field at
  bit 2 of byte 8 rather than a member of a 14-byte blob.
- **Conversions**: five of MDF's eleven map onto §7 exactly. `tab` (nearest key)
  needs its keys moved to the midpoints to become §7's floor lookup — exactly,
  not just on the keys. **Algebraic gets nothing**, deliberately: it is a text
  formula the reader is expected to evaluate, and §7 rejects that. A
  value-to-text table whose default is itself a conversion cannot survive whole,
  so the numbers win and the names become field metadata. Everything unmapped is
  reported through `Options.Warn`; nothing is dropped in silence.
- **Unfinalized files** — a logger stopped mid-write, cycle counts never
  patched — read fine; the record count comes from walking the data, and a
  partial trailing record is dropped rather than padded.
- A **virtual master** becomes an implicit axis, the one case where the converted
  record is *smaller* than the one it came from.

`axis_exp` stops at nanoseconds here, unlike SPICE: a measurement's timestamps
come from a clock, and resolving them finer claims precision the instrument did
not have while costing the axis its `time.Duration`.

**The DBC decoder is what makes a bus recording worth plotting.** An MDF bus log
contains frames, not signals — `EngineSpeed` is not in the file and never was, it
is in a database — so `mdf2logb -dbc` writes a stream per message beside the raw
frames. Two claims of the format are cashed here rather than asserted:

- A **multiplexed** signal becomes a §6.2 guard. In the OBD2 fixture ten signals
  share bits 88–103 of `OBD2_Response` and `PID` selects which is live; the test
  checks not only that the selected one decodes correctly but that **the other
  nine report absent**, which is the difference between this and every tool that
  returns a number for all ten.
- A **Motorola** signal is `8*(start/8) + (7 - start%8)` and nothing else, which
  is CAN.md's central claim. `EngineSpeed` is `31|16@0+` and lands at payload bit
  24 with no data movement.

`TestDecodedSignals` recomputes six signals over 628 frames straight from the
OBD2 formulas and compares; the full recording yields 11 517 responses and an
engine-speed trace running from an 850 rpm idle to 3 586 rpm. Extended
multiplexing (`SG_MUL_VAL_`, and a multiplexor that is itself multiplexed) is
**refused**, because Logb's guards do not chain by design and a signal decoded out
of frames that do not carry it is worse than a missing one.

**The database is embedded with the data it explains** — an ATTACH frame plus
`source.dbc`, `source.dbc.sha256`, and `dbc.database` on every decoded stream,
the same move `raw2logb` makes with a netlist. This is the honest answer to the
one thing about bus recordings that no container solves by being clever:

> A CAN recording is not self-explanatory in *any* format. It holds frames; what
> they mean lives in a database owned by whoever built the vehicle, and it is not
> on the wire. The files here do not carry it — `testdata/mdf/obd2-trunc.mf4` has
> no attachments at all, and the only clue to what the recording is about is the
> free text `Peugeot208` in an XML header comment. But a Logb file converted
> *without* `-dbc` is in exactly the same position, so this is not a point the
> format wins on capability. It wins on convention: decode at import, and carry
> the database in.

> [!NOTE]
> **To be verified: what MDF4 says about attaching a bus database.** The claim
> above is about the files in `testdata/mdf`, not about the standard, and the
> difference matters. What is established: MDF4 has AT blocks that carry an
> arbitrary file with a filename and a MIME type — `sample3.mf4` embeds one, and
> `mdf/mdf.go` reads them — so a logger *could* put a DBC in a recording. What is
> **not** established, because the ASAM spec is paywalled and this project works
> only from public sources (README.md:36):
>
> - whether the bus-logging part of MDF 4.x defines a **convention** for it — a
>   reserved MIME type, an expected filename, a link from a CAN channel group to
>   the AT block that describes it — or whether embedding one is merely possible
>   and unstandardised;
> - whether any tool in the ecosystem looks for one;
> - whether MDF 4.3's associated-standard mechanism (the one SPEC.md §9.1 cites
>   for GNSS) covers bus descriptions too.
>
> Until someone checks this against the standard, "MDF does not carry the
> database" should be read as *these recordings do not*, which is all the
> evidence here supports. If a convention does exist, `mdf2logb` should look for
> the attachment and use it when `-dbc` is absent — that would be a strictly
> better default than asking the user for a file the recording already has.

`TestDatabaseTravelsWithTheData` checks that the embedded copy is byte-identical
and still parses, so the file can be re-decoded from itself.

70 tests, all passing (99 counting subtests).

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
4. ~~**Consider an MDF4 importer**~~ — done; see `mdf/` above. The reader is this
   repository's own rather than `/files/go/src/golib/formats/mdf/mdf.go`, which
   was the starting point for the block layer but is built around per-channel
   `[]any` samples and drops what an importer needs: `sync_type`, composition,
   VLSD, attachments, and eight of MDF's eleven conversion types.

   Still open there, in rough order of how much it would matter:

   - **Whether MDF4 defines a convention for attaching a bus database**, and if
     so, reading it so `-dbc` is only needed when the recording lacks one. See
     the note above; this is a question about the standard, not the code, and it
     is the one whose answer would change behaviour rather than add a feature.
   - **MDF 3.** A different container — two-byte block ids, no `##` magic, a
     different link layout — so a second parser of comparable size, and there is
     no v3 file here to check it against.
   - **Event blocks.** MDF's EV is a timestamped annotation on a recording, and
     Logb has nowhere to put one. Probably a stream of its own rather than a new
     frame type, but that is a spec question, not an importer one.
   - **Channel arrays.** A CA composition is kept as one opaque field today.
   - **Streaming.** `ReadFile` materialises the whole recording; a gigabyte file
     needs a gigabyte. Fine for an importer that touches every byte once, wrong
     for anything else.

## Things not yet implemented

- The INDEX frame is written and skipped on read. No seek API exists; the reader
  is single-pass by design and the index is a pure accelerator.
- Codec lz4 is specified but unimplemented. zstd (the default) and deflate
  both work; an lz4 frame is rejected and recorded in `Unsupported`, which is
  §8's defined behaviour rather than a gap in it.
- `cmd/logbdump` has no golden test of its own output. The fixture pins the
  bytes; nothing pins the rendering.
