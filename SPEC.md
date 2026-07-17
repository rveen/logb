# Logb

**Version 0.1 (draft).** A self-describing binary format for time-series
measurement, bus-trace, and simulation recording. Designed to be written by
embedded loggers and read by analysis tools.

*Logb* is `log` plus `b` for binary — the convention that makes `.xlsb` the binary
twin of `.xlsx` and `jsonb` the binary form of JSON.

The design is a **varve**: one season's sediment couplet in a lake bed, laid down
in sequence, never rewritten, each one self-dating, and readable from any cut face
of the core — including a core that snapped. Geologists date events by counting
them. That is this format's design rather than a decoration on it, and §1 is the
same list stated as rules.

File extension `.logb`. Status: draft for discussion.

## 1. Design rules

These are the rules the rest of the spec is accountable to. If a proposed feature
breaks one, the feature loses.

1. **Nothing points forward.** A frame may reference earlier bytes; never later
   ones. A writer never seeks back to patch a field it already emitted.
2. **Append-only, crash-safe.** A file truncated at an arbitrary byte — power loss
   mid-write — is a valid file containing every record up to the last intact
   frame. No repair tool, no "recovery mode".
3. **Cut anywhere, decode.** A reader handed the middle of a file, with no access
   to the start, can resynchronise and decode records with full schema. Schema is
   repeated, not stated once.
4. **No dependencies.** A conforming reader is implementable in ~1000 lines with
   only a decompressor. No XML, no external schema registry, no library that must
   still exist in 2050.
5. **Raw is preserved.** Stored values are the bits the sensor or bus produced.
   Physical values are derived by a declared conversion. A read-modify-write
   round trip is byte-identical.
6. **Fixed cost per record.** Adding a channel must not change the cost of
   decoding an unrelated channel.

## 2. Conventions

All multi-byte integers in the format's own structures — frame headers, schema
fields, lengths — are **little-endian**, always. A *record's* fields may declare
either byte order, because bus payloads mix them (§6.3); bit numbering within a
record follows that byte order, and the rule is stated once, in §6.2. Strings are
UTF-8, length-prefixed with a `u32`, not NUL-terminated. `i64` timestamps are
nanoseconds.

`crc32c` is CRC-32 with the Castagnoli polynomial (0x1EDC6F41), which has
hardware support on every target that matters.

## 3. File structure

```
file    := file-header segment* [index-frame] [end-frame]
segment := sync-frame schema-frame+ run-frame* (meta | attach | data)*
```

A **segment** is a self-contained decode unit: it restates every schema needed by
the data frames it contains. A writer starts a new segment periodically (every
N megabytes or M seconds — a writer policy, not a spec requirement). The cost is
one schema restatement per segment; the benefit is rule 3.

The index and end frames are optional by construction: a file that lost power
simply lacks them, and rule 2 holds.

### 3.1 File header (16 bytes)

```
+0   8   magic       \x89 L O G B \r \n \x1a
+8   2   version_major = 0
+10  2   version_minor = 1
+12  4   crc32c of bytes 0..11
```

The magic follows PNG's design: the high bit catches 7-bit-clean transports, the
`\r\n` pair catches line-ending mangling, `\x1a` stops `type` on DOS.

PNG spends a ninth byte on a trailing `\n`, to catch the LF→CRLF direction that
its `\r\n` pair does not. This spends that byte on the fourth letter of the name
instead, and loses less than it appears: under LF→CRLF the `\r\n` in the magic
becomes `\r\r\n` and fails the comparison anyway. The two guards overlap, and the
header stays 16 bytes.

Readers MUST reject a file whose `version_major` they do not know. Readers MUST
accept an unknown higher `version_minor` and skip frame types they do not
recognise (§4.2).

### 3.2 Frame

Every frame after the file header has the same shape:

```
+0   4   payload_len   u32, bytes of payload only
+4   1   frame_type    u8
+5   1   flags         u8
+6   2   stream_id     u16, 0 if not stream-scoped
+8   n   payload
+8+n 4   crc32c        over the 8-byte header and the payload
```

12 bytes of overhead per frame. Frames are batches, not records, so this is
noise. A reader validates the CRC before trusting any payload byte; a frame whose
CRC fails, or whose `payload_len` runs past end-of-file, terminates the read at
that point (rule 2).

### 3.3 Frame types

| ID   | Name    | Scope  | Purpose |
|------|---------|--------|---------|
| 0x01 | SYNC    | file   | Segment boundary; resynchronisation point |
| 0x10 | SCHEMA  | stream | Stream definition: fields, layout, conversions |
| 0x11 | META    | either | Key/value metadata |
| 0x12 | ATTACH  | file   | Embedded file (DBC, calibration, config, netlist) |
| 0x13 | RUN     | file   | Declares a run: parameter set for a swept/repeated dataset |
| 0x20 | DATA    | stream | A batch of records |
| 0x30 | INDEX   | file   | Offsets for random access |
| 0x40 | END     | file   | Records that a writer closed cleanly here |
| 0x50 | SIGN    | file   | **Reserved.** Not defined in v0.1 |

Unknown frame types MUST be skipped using `payload_len`. This is the only
extension mechanism, and it is enough.

**0x50 is reserved for a signature over the preceding segment**, and nothing about
it is defined here beyond the id. Reserving it costs a table row and stops the id
being taken for something else; defining it would mean specifying key
distribution, which is not this format's business.

The reservation exists because a *detached* signature — a `.sig` file over the
whole thing — contradicts the two properties this format is built on. It is void
the moment the file is truncated by power loss (rule 2) or appended to another
file (§6.6), and those are precisely the two operations Logb promises leave a
valid file behind. A signature frame covering the bytes since the previous SYNC
frame survives both: truncation drops the last, unsigned segment and leaves every
earlier segment's signature intact, and concatenation carries each file's
signatures along with its bytes. Per-segment is therefore the only shape that fits,
and it fits without argument — it is a backwards reference, which rule 1 permits.

**Encryption is not reserved and is not coming.** Logb is an open format; a file's
confidentiality is the business of the layer that stores or moves it, where it can
be solved once for every file type rather than badly for this one.

**An END frame does not end the read.** It states that a writer closed cleanly at
that point — a statement about the past, not a command about the future — and has
no more authority over a reader than the index does (§9). A reader that finds more
bytes after an END frame MUST continue scanning. This is what a concatenated file
looks like (§6.6): END frames in the middle, marking where each original file
ended. A reader stops at end-of-input or at damage, never at a frame that asks it
to.

## 4. Resynchronisation

### 4.1 SYNC frame

Payload is a fixed 16-byte pattern followed by segment bookkeeping:

```
+0   16  sync_pattern  4C 4F 47 42 53 59 4E 43 A7 3E 91 D2 5C 68 0B F4
+16  8   segment_seq   u64, monotonic from 0
+24  8   wall_time_ns  i64, wall clock at segment start, 0 if unknown
```

`wall_time_ns` is a coarse seek hint only — it says when the segment was written,
not what its streams' axes mean. A simulation file's segments carry a wall time
while their streams' axes carry simulation time; the two are unrelated, and that is
the point of keeping this out of §5.

A reader that has lost framing scans for `sync_pattern`, backs up 8 bytes to the
frame header, validates the CRC, and is synchronised. The pattern is 16 bytes so
that a false positive in random data is not a practical concern, and the CRC check
rejects one anyway.

### 4.2 Forward compatibility

The combination of length-prefixed frames, CRC validation, and per-segment schema
restatement means a v0.1 reader can decode a v0.9 file's v0.1 streams, ignoring
what it does not understand. This is the property MDF4's link-graph cannot
provide: there, an unknown block in a linked list is a dead end.

## 5. The domain axis

Every stream is indexed by one independent variable — its **axis**. For a data
logger that is wall-clock time. For a transient simulation it is simulation time,
which is not the same thing. For an AC sweep it is frequency; for a DC sweep, the
value of the swept source; for a rotational measurement, crank angle.

MDF4 models this as a "master channel" you have to go find among the ordinary
channels, but it does at least admit that the master need not be time. Logb makes
the axis explicit and non-optional.

A stream declares:

```
axis_kind   u8   0=time, 1=frequency, 2=angle, 3=distance, 4=index, 5=other
axis_mode   u8   0=implicit uniform, 1=explicit field, 2=implicit log. §5.3
axis_exp    i8   time only: the tick, as a power of ten of a second. §5.1
axis_unit        UTF-8, e.g. "s", "Hz", "V", "deg"
```

**Implicit uniform** — records are equally spaced; the record's axis value is
`axis_base + i * axis_step`. Zero bytes per record. This is periodic sampling, and
a uniform simulation output.

**Explicit** — one field carries the axis value, as an offset from the DATA
frame's `axis_base` scaled by the schema's `axis_scale`. A logger recording CAN at
microsecond resolution over a one-hour segment fits its timestamps in a `u32`; a
simulator with adaptive timesteps writes an `f64` and pays for what it uses.

**Implicit log** — records are equally spaced in log space; the axis value is
`axis_base * ratio^i`. Zero bytes per record, for the sweep that is uniform in no
other coordinate. §5.3.

**A reader MUST reject a schema whose `axis_mode` it does not know, and MUST skip
that stream rather than the file.** This is §4.2's rule applied one level down, and
it is not optional politeness: every mode computes the axis from different fields,
so a reader that falls back on a default reports a wrong axis for every record of
that stream and says nothing. Skipping the stream leaves every other stream in the
file readable, which is the whole promise of §4.2 — a v0.1 reader decodes a v0.9
file's v0.1 streams. This is what makes a future `axis_mode` a safe addition rather
than a silent hazard, exactly as an unknown `codec` (§8) is safe today.

Note the asymmetry with an unknown `codec` (§8), which costs only its own frame:
the rest of the file still decodes. An axis is not a frame. There is no partial
answer — a stream whose axis cannot be computed has nothing worth returning.

`axis_base` (8 bytes, in the DATA frame) and `axis_step` / `axis_scale` (8 bytes,
in the schema) are **interpreted according to `axis_kind`**:

- `axis_kind = time` — `i64` counts of the **tick** defined by `axis_exp` (§5.1).
  Integer, exact, no accumulated rounding over a long recording or a long
  simulation. This is the fast path and it stays cheap.
- any other kind — IEEE `f64`, in `axis_unit`. A frequency sweep from 10 Hz to
  1 MHz has no business being expressed in nanoseconds. `axis_exp` is ignored;
  writers SHOULD emit 0.

### 5.1 The tick: `axis_exp`

For `axis_kind = time`, every integer axis quantity is a count of ticks, where one
tick is 10<sup>`axis_exp`</sup> seconds. A reader recovers seconds as:

```
implicit:  t = (axis_base + i * axis_step)          * 10^axis_exp
explicit:  t = (axis_base + field * axis_scale)     * 10^axis_exp
```

`axis_exp = -9` is nanoseconds and is the default a logger should write.
`axis_exp = -15` is femtoseconds, which is what an RF transient analysis needs.
The two coefficients compose and do different jobs: `axis_exp` sets the unit,
`axis_scale` lets a narrow field count in multiples of it (a `u32` field with
`axis_exp = -15` and `axis_scale = 1000` covers 4.3 µs of femtosecond-resolution
timestamps in four bytes per record).

This is the same device as VCD's `$timescale`, which EDA tooling has used for forty
years, minus VCD's redundant 1/10/100 multiplier — a power of ten is strictly more
expressive per byte.

**The exponent trades resolution against range, and `i64` is the budget:**

| `axis_exp` | tick | `i64` span |
|-----|------|------------|
| -6  | µs | ±292,471 years |
| -9  | ns | ±292 years |
| -12 | ps | ±106.8 days |
| -15 | fs | ±2.56 hours |
| -18 | as | ±9.2 seconds |

Nothing here is a limit in practice, because **the span you need is set by the time
base, not by the file size** (§5.2):

- `time.base = sim` — the origin is 0, so the span is the *duration of the
  simulation*. 2.56 hours of femtosecond-exact transient is orders of magnitude
  more than any circuit simulation will ever produce. Attoseconds still buy you
  9.2 seconds, which is also fine.
- `time.base = unix` — the span must reach back to 1970, so nanoseconds is the
  practical floor. Femtoseconds would run out in 1970.
- `time.base = monotonic` — the origin is arbitrary, so a picosecond-resolution
  logger picks `axis_exp = -12`, gets 106 days of span, and recovers wall clock
  through anchors (§5.2). **This is the combination that makes sub-nanosecond
  logging work**, and it falls out of machinery that was already there for
  loggers that boot without an RTC.

A reader MUST NOT assume `axis_exp = -9`. Converting ticks to a language-native
duration type is where implementations will get this wrong: Go's `time.Duration` is
`int64` nanoseconds and **cannot represent a femtosecond tick at all**. A
conforming reader exposes the raw tick count and the exponent, and only offers a
`Duration` when `axis_exp >= -9`.

### 5.2 Time base and late clocks

For `axis_kind = time`, the meaning of the epoch is declared by the file's
`time.base` metadata key:

- `unix` — nanoseconds since 1970-01-01T00:00:00Z, UTC, leap seconds smeared.
- `tai` — nanoseconds since the TAI epoch. Correct, and what you want if you are
  going to correlate with anything scientific.
- `monotonic` — an arbitrary origin. **For loggers with no RTC at boot.** The
  origin is unknown at the time the first records are written.

- `sim` — simulation time. An origin of zero with no relation to wall clock.
  A transient analysis starting at t=0 is not claiming to have happened in 1970.

A `monotonic` file may later be bound to wall-clock time by a META frame carrying
`time.anchor` = `<monotonic_ns>:<unix_ns>`, emitted whenever the logger acquires a
GPS or NTP fix. This is emitted *after* the records it retroactively dates, which
is exactly what rule 1 permits and what MDF4 cannot express — there, a logger that
boots without a clock has to either lie in the header or seek back and rewrite it.

Multiple anchors are allowed; a reader fits them to recover clock drift. A file
may legitimately have zero anchors, in which case its records are ordered and
relatively timed but not dated.

Both sides of an anchor are **nanoseconds regardless of the stream's `axis_exp`**,
because no wall-clock source justifies better: a GPS fix is tens of nanoseconds at
best, and NTP is milliseconds. A picosecond-tick stream converts its ticks to ns to
apply the anchor and loses nothing real — the precision is in the *intervals*
between its records, which keep full tick exactness, not in the absolute date the
anchor supplies.

### 5.3 The log-spaced axis

An AC sweep from 10 Hz to 1 MHz at ten points per decade is not uniform in
frequency and never will be. It is uniform in the logarithm of frequency, which is
the coordinate the instrument actually swept and the coordinate the plot is drawn
in. `axis_mode = 2` says so:

```
axis = axis_base * ratio^i
```

`ratio` is carried in `axis_step`, as an IEEE `f64` like every other non-time axis
quantity (§5). A sweep of *n* points per decade has `ratio = 10^(1/n)`; per octave,
`2^(1/n)`. Cost is zero bytes per record, the same as implicit uniform, for a sweep
that would otherwise have to write an `f64` per point purely to restate what its
own definition already fixed.

**Undefined for `axis_kind = time`, and a reader MUST reject the combination.**
Time is an integer count of ticks (§5.1); a log-spaced tick count is not one, and
the exactness that §5.1 exists to protect would not survive the attempt. Nothing
sweeps time logarithmically anyway.

`axis_base` MUST NOT be zero — every point would be zero — and `ratio` must be
finite, positive, and not 1. A reader computes `axis_base * pow(ratio, i)` rather
than multiplying its way along the sweep, so that the last decade of a wide sweep
is as accurate as the first.

This is what SPICE writes as `Flags: log` (§11). LTspice stores such a sweep's axis
explicitly regardless, so an importer may keep doing that; the mode is here because
adding it after 0.1 freezes would be a breaking change, and the cost of having it
is one branch in `AxisAt`.

## 6. Streams and schema

A **stream** is a named sequence of records sharing one layout — MDF4's channel
group, without the data-group indirection.

A stream has two identifiers, and keeping them separate is what makes files
concatenable (§6.6):

- **`stream_id`** — a `u16` routing tag in every frame header, saying which schema
  decodes this frame. **Scoped to the segment**, not the file: it is bound by the
  SCHEMA frames following each SYNC frame, and that binding expires at the next
  SYNC frame.
- **`stream_uuid`** — 16 opaque bytes in the SCHEMA frame, stating which logical
  stream this is. Stable across segments, files, and writers.

### 6.1 SCHEMA frame payload

```
+0   16  stream_uuid            opaque; identity across segments and files, §6.6
     4   name_len + name        UTF-8 stream name
     4   record_bits            u32, fixed portion, bit-exact
     1   axis_kind              §5
     1   axis_mode              0=implicit, 1=explicit, 2=implicit log
     1   axis_exp               i8, time only; the tick, §5.1
     1   reserved
     4   axis_unit_len + unit   UTF-8
     8   axis_step              implicit: the step, i64 ticks or f64 per axis_kind
                                implicit log: the ratio, f64 (§5.3)
     8   axis_scale             explicit only; i64 ticks or f64 per axis_kind
     2   axis_field             explicit only, index into fields
     2   field_count            u16
     n   field[]                see §6.2
     4   meta_count + meta[]    inline key/value pairs
```

`record_bits` is a `u32`: a record's fixed portion may be up to 512 MiB, which is
already several orders of magnitude past any real layout. It was a `u64` in an
earlier draft, permitting a 2-exabit record — a number with no relation to
anything, in a field every schema pays for.

Within a segment, a `stream_id` MUST carry exactly one schema. Across segments, the
same `stream_uuid` MUST carry an identical schema — a schema change means a new
`stream_uuid`. This keeps readers simple and makes "the schema changed halfway
through" impossible to express, which is a feature.

A reader accumulating a stream across segments matches on `stream_uuid`, never on
`stream_id`. It already reads every SCHEMA frame at every sync point, so this costs
it nothing.

### 6.2 Field

```
     4   name_len + name
     4   bit_offset     u32, from start of record
     4   bit_width      u32
     1   data_type      §6.3
     1   byte_order     0=little, 1=big
     1   flags          bit0: variable-length (payload in tail, §6.4)
     4   unit_len + unit        UTF-8, e.g. "km/h", "" if dimensionless
     4   desc_len + desc
     n   conversion             §7
```

Fields may overlap and need not be byte-aligned. A 1-bit flag at bit offset 37 of
a 64-bit CAN payload is expressible directly, which is the whole reason the
bit-level model exists and why an Arrow-based substrate could not have worked.

#### Bit numbering

**Bit numbering follows the byte order.** That is the entire rule, and it has no
exceptions:

- **`byte_order = little`** — bit *n* is byte *n/8*, bit *n%8* **counting from the
  least significant bit** of that byte.
- **`byte_order = big`** — bit *n* is byte *n/8*, bit *n%8* **counting from the
  most significant bit** of that byte.

In both cases `bit_offset` names the field's **first bit** and the field is
`bit_width` bits running **upward** from it. A signed field is the extracted slice
read as two's complement. There is no alignment requirement, no jump, and no
special case for a field that crosses a byte boundary.

The rule works because **each byte order's fields are contiguous in that order's
own numbering** — which is the fact the whole design turns on. Little-endian
signals run contiguously from the LSB; big-endian signals run contiguously from
the MSB. The conventional way to state this — one flat numbering, plus a rule for
how big-endian fields walk through it — is what produces the notorious "Motorola
sawtooth", where a signal descends through a byte then jumps to the top of the
next. The sawtooth is an artefact of describing big-endian fields in
little-endian numbering. Number each in its own terms and it disappears.

**The big-endian half is exactly DBC's Motorola convention**, which is the point:
a DBC importer converts a start bit with

```
bit_offset = 8*(start_bit/8) + (7 - start_bit%8)
```

and is done. No data moves, nothing is lost, and every Motorola signal — aligned
or not — is a Logb field. This was verified against Vector's reference algorithm
over 465,600 combinations of signal position, width, and payload, with zero
disagreements; the check lives in `TestDBCMotorola`.

The alternative of mandating a single byte order was considered and rejected. It
does not relocate the work into importers — it makes the data inexpressible. Of
the 1,552 Motorola signals that fit an 8-byte CAN frame, only 288 have any
little-endian bit-slice equivalent, and **not one of those is wider than a single
byte**: a plain byte-aligned 16-bit Motorola signal has no little-endian
expression, because both orders read the same bits and disagree about which byte
carries the high half. Byte order is not a numbering convention layered over the
bit model; it is a second thing, and a format for bus data has to carry both.

#### Conformance vectors

Normative. An implementation that reproduces this table has the rule right; one
that does not, does not. **These vectors, not the prose above, are the
specification.** MDF4 and DBC do not disagree about bit numbering because anyone
was careless — they disagree because this is genuinely hard to say in English, and
every prose statement of it that has ever been written, including earlier drafts
of this section, has been ambiguous or wrong. Vectors do not have that failure
mode.

| Record bytes | `byte_order` | `bit_offset` | `bit_width` | Raw value | As `sint` |
|---|---|---|---|---|---|
| `12 34` | little | 0 | 16 | `0x3412` | |
| `12 34` | big | 0 | 16 | `0x1234` | |
| `12 34` | little | 4 | 12 | `0x341` | |
| `12 34` | big | 3 | 12 | `0x91A` | |
| `00 20` | little | 13 | 1 | `1` | |
| `00 20` | big | 10 | 1 | `1` | |
| `78 56 34 12` | little | 0 | 32 | `0x12345678` | |
| `12 34 56 78` | big | 0 | 32 | `0x12345678` | |
| `FF 0F` | little | 0 | 12 | `0xFFF` | `-1` |
| `FF F0` | big | 0 | 12 | `0xFFF` | `-1` |
| `A5` | little | 1 | 6 | `0x12` | |
| `A5` | big | 1 | 6 | `0x12` | |

The fourth row is the one that matters: a big-endian field, unaligned, crossing a
byte boundary — the case that has no defined meaning in MDF4 and that every CAN
tool carries scar tissue from. `TestConformanceVectors` is this table.

### 6.3 Data types

| ID | Type | Notes |
|----|------|-------|
| 0 | `uint` | 1..64 bits |
| 1 | `sint` | 1..64 bits, two's complement, sign-extended on decode |
| 2 | `float` | 16, 32, or 64 bits, IEEE 754 |
| 3 | `bool` | 1 bit |
| 4 | `bytes` | whole bytes in the record, or variable-length in the tail (§6.4) |
| 5 | `string` | as `bytes`, UTF-8 |
| 6 | `complex` | 64 or 128 bits: two IEEE floats, real then imaginary |

**`bytes` and `string` come in both forms, and the fixed one is the important
one.** A field with `flags` bit 0 clear is a slice of whole bytes inside the
record, at a byte-aligned `bit_offset` with a byte-sized `bit_width`; a reader
MUST reject any other alignment, because a blob at bit 3 has no byte to return.
This is what a CAN payload is (§6.4), and it is the common case. Setting bit 0
moves the field's bytes to the tail and costs the batch its seekability — see
§6.4, which is where the length prefix lives.

Seven types. MDF4 has fifteen, including three string encodings and a "canopen
date" that exists because someone needed it in 1997. UTF-16 and Latin-1 channels
convert to UTF-8 on import; a byte-exact round trip of a foreign format's string
encoding is that format's importer's problem, not this format's.

`complex` is first-class rather than two adjacent float fields, because in an AC
analysis the real and imaginary parts are one measured quantity: they share a unit,
a conversion, and a name, and every tool that plots them wants magnitude and phase.
Splitting them into two fields would push reassembly into every reader — the same
mistake as LTspice's unmarked runs (§6.5), one layer down.

`byte_order` is per-field because a single CAN frame routinely mixes Intel and
Motorola signals, and any format that gets this wrong is unusable for bus data.
Both orders are fully defined at every offset and width, including unaligned and
byte-crossing fields; see §6.2's numbering rule and its conformance vectors.

### 6.4 Variable-length fields

A DATA frame is every record's fixed portion, and then one tail region:

```
[record 0 fixed] [record 1 fixed] ... [record N-1 fixed] [tail 0] [tail 1] ... [tail N-1]
```

**All of the fixed portions come first — the tails are not interleaved with the
records they belong to.** This is not a preference. `filter=transpose` (§8) groups
byte *i* of every record together, which requires the fixed portions to be
contiguous and equally sized; interleaved tails would make the fixed region
neither. Transpose therefore covers the fixed region only, and the tail region is
appended to it untransposed.

Record *i*'s tail is one `u32` length followed by that many bytes, per
variable-length field, in field-declaration order. A field with no bytes writes a
zero length; it is not omitted.

**A variable-length field occupies no fixed bits.** Its `bit_width` MUST be 0, it
contributes nothing to `record_bits`, and its `bit_offset` is ignored and SHOULD
be 0. There is no pointer in the fixed portion — nothing to point with, and rule 1
would forbid it anyway.

The tail is parsed sequentially, so record *i*'s bytes can be found only by
walking records 0..*i*−1. That is the cost, and it is deliberate: **a
variable-length field costs the batch its seekability.** It is why bus payloads
are fixed-width `bytes` fields (§6.3) and not variable ones — a CAN payload is
always eight bytes, and paying a length prefix plus a linear walk to say so would
be a poor trade. Variable fields exist for log strings and blobs, where the length
genuinely varies and there is nothing to seek to.

### 6.5 Runs

A **run** is one dataset within a stream: the same schema, measured or simulated
again under different conditions. A `.step` parameter sweep, a Monte Carlo batch,
a repeated test cycle, a corner analysis. Each run's axis restarts.

Every DATA frame carries a `run_id` (u32). A RUN frame declares what a `run_id`
means:

```
+0   4   run_id         u32
     4   index          u32, ordinal within the sweep
     4   param_count + param[]   key/value, e.g. "R1"="1.0e3", "temp"="27"
```

A logger writes `run_id = 0` forever and never emits a RUN frame; the concept costs
it four bytes per batch and no complexity. A stepped simulation emits one RUN frame
per step, restated per segment like schemas (§3).

**This exists because LTspice's raw format doesn't have it.** A `.step` sweep
concatenates every run into one file with no boundary marker, so every consumer is
reduced to guessing — `rveen/ltspice`'s own analyzer recovers run boundaries by
testing whether the time axis has returned to zero:

```go
// detect LT runs (time == 0)
if i > 0 && m[0][i] == 0 {
```

That heuristic is wrong for any run whose axis legitimately revisits zero — a DC
sweep from -5 V to +5 V, an AC sweep that includes DC, a transient with a negative
start time. The information exists at write time and is thrown away. A format whose
readers must guess at dataset boundaries has failed at the one job a self-describing
format has.

Runs are not streams: a thousand-point Monte Carlo is one schema and a thousand
`run_id`s, not a thousand schemas. This is also why `run_id` is a `u32` while
`stream_id` is a `u16` — sweeps get large, channel counts don't.

`run_id` follows the same scoping rule as `stream_id`: segment-scoped, rebound by
the RUN frames after each SYNC frame. A run's identity across segments is its
`index` and parameter set, which is what a reader groups by.

**Runs are contiguous. Within one segment, a stream's DATA frames MUST be grouped
by run: once the `run_id` on a stream's frames changes, the previous `run_id` MUST
NOT reappear for that stream before the next SYNC frame.** A run may span segments
freely — scope is rebound at each SYNC frame, and a long run simply resumes on the
other side.

The alternative was to let `run_id` mean whatever the writer felt like per frame,
which permits a file whose runs are shuffled. Nothing wants to read that file. A
streaming consumer that cannot assume contiguity has to keep every run open until
end of segment, on the chance that run 3 comes back after run 700 — so a rule
costing writers nothing in the normal case buys every reader the right to close a
run out when the id changes.

The case that pays for it is a real-time multi-corner simulator, which has several
runs genuinely in flight at once. It buffers per run, or it starts a new segment
per batch of frames. That is the same answer §10 gives MDF4's unsorted records, for
the same reason: the writer knows how to un-shuffle its own output, and it is the
only party in the system that does.

The rule is a constraint on writers, not a licence for readers to corrupt: a reader
that meets a violation MAY reject the file, but MUST NOT silently attribute records
to the wrong run. Grouping by `run_id` — the obvious implementation — is already
tolerant of a violation and is what a reader should do.

### 6.6 Concatenation

**Two Logb files concatenate by appending the second file's bytes minus its 16-byte
header.** No rewriting, no id remapping, no index merge. The result is a valid file.

This works because `stream_id` is segment-scoped. The second file's first SYNC
frame rebinds every id, so file B reusing `stream_id = 1` for a different stream
than file A is not a collision — it is just a new binding, which is what a SYNC
frame means. The alternative designs both fail here: a file-scoped `u16` makes
concatenation a rewrite, and a per-frame UUID makes every DATA frame 14 bytes
fatter to solve a problem that only arises once per segment.

Whether two streams *merge* on concatenation is then decided entirely by
`stream_uuid`, which is the writer's call and cannot be inferred:

- **A logger rolling over to a new file** persists its uuids, so its hourly files
  concatenate into one continuous recording.
- **Two loggers with identical configuration** — a left and a right sensor box —
  generate different uuids, so their files concatenate into one file with two
  distinct streams rather than one falsely-merged mess.

This is exactly why the spec does not mandate how to derive the uuid, and why it is
not a content hash of the schema. A content hash would force the second case to
merge, because the two boxes' schemas are byte-identical; nothing in the data
distinguishes them, and only the writer knows they are different instruments.
Suggested writer policy: UUIDv5 over (device serial, stream name) where streams
should merge across files, random UUIDv4 where they should not. The spec's only
rule is that equal uuid means same logical stream, and it is the writer's job to
make that true.

## 7. Conversion

Raw stored value → physical value. The conversion is part of the field, encoded as
a tagged struct: `u8 type`, then type-specific parameters.

| ID | Type | Parameters | Physical value |
|----|------|-----------|----------------|
| 0 | identity | — | `x` |
| 1 | linear | `f64 a, b` | `a + b*x` |
| 2 | rational | `f64 p1..p6` | `(p1x² + p2x + p3) / (p4x² + p5x + p6)` |
| 3 | table | `u32 n`, `n × (f64 key, f64 val)` | lookup, no interpolation |
| 4 | table_interp | `u32 n`, `n × (f64 key, f64 val)` | lookup, linear interpolation |
| 5 | value_to_text | `u32 n`, `n × (f64 key, string)`, default string | enum decode |
| 6 | range_to_text | `u32 n`, `n × (f64 lo, f64 hi, string)`, default | range decode |

Seven conversions, all closed-form and all decodable by a reader with no parser.

**`identity` is type-preserving.** It means "this value is already physical", not
"coerce it to a float". A `bool` field under `identity` reads back as a bool, a
`string` field as a string, a `uint` as an unsigned integer. Only the six
non-identity conversions take a numeric input and are defined solely for the
numeric types (`uint`, `sint`, `float`, `bool`, `complex`).

**A non-identity conversion on a `bytes` or `string` field is invalid, and a
reader MUST reject the schema rather than the value.** There is no sensible
failure at read time: a reader that meets `linear` on a string has no number to
apply it to, and the tempting response — hand back the string unconverted — is the
worst one available, because it silently produces a value that looks converted and
is not.

This looks obvious written down and is easy to get wrong in code: a reader that
routes every field through a uniform `raw → float64 → convert` pipeline turns
every bool channel into a float. MDF4 has this disease. The rule is that the
pipeline is entered only when a conversion is actually present.

For `complex` fields, `identity`, `linear`, and `rational` apply component-wise
with real coefficients; the table and text conversions are undefined and MUST be
rejected by a reader.

**Deliberately excluded: MDF4's algebraic conversion**, which embeds a text formula
the reader must parse and evaluate. It is a scripting language smuggled into a data
format, every implementation disagrees about its grammar, and it is a remote code
execution surface in a file you were handed by a third party. A conversion that
cannot be expressed as rational or a table belongs in the analysis tool.

## 8. DATA frame

```
+0   8   axis_base      i64 ns or f64, per the stream's axis_kind (§5)
+8   4   record_count   u32
+12  4   run_id         u32, 0 if the stream has no runs (§6.5)
+16  1   codec          0=none, 1=zstd, 2=lz4, 3=deflate
+17  1   filter         0=none, 1=transpose
+18  2   reserved
+20  8   raw_size       u64, payload size after decode, for one-shot allocation
+28  n   records        record_count fixed portions, then the tail region (§6.4)
```

`filter=transpose` groups byte *i* of every record together before compression —
MDF4's one genuinely good idea. Column-ish locality on a row-major layout, which
typically triples the compression ratio on slowly-varying sensor data for a
transform that is twenty lines of code. Transpose applies to the fixed region
only; the tail region is appended to it untransposed, and this is why §6.4 puts
every fixed portion before every tail rather than interleaving them.

Default codec is **zstd**. MDF4 predates it and is stuck with deflate.

**A reader that meets a `codec` or `filter` it does not know MUST reject that
frame, and MUST NOT return its records.** Unlike an unknown frame type, which is
skippable because its meaning is unknown, an unknown codec has a perfectly clear
meaning the reader simply cannot carry out; guessing would mean handing back
compressed bytes as records. This is what makes a future codec a safe addition —
an old reader fails on the new frames and says why — and it is the property that
lets §12.7 defer compression dictionaries indefinitely at no cost.

Because each DATA frame carries its own `axis_base` and `run_id`, a frame is
independently decodable given its schema, and that is what makes §4 work.

`filter=transpose` also happens to be exactly what LTspice's `Flags: fastaccess`
does — column-major rewriting of a row-major file — except that LTspice makes it a
whole-file mode you opt into at write time, whereas here it is per-frame and
invisible to the reader.

## 9. INDEX frame

Written at clean close. Grouped by `stream_uuid`, not `stream_id` — a
segment-scoped tag is meaningless in a file-scoped structure, and grouping also
keeps the 16 bytes of uuid out of the per-frame entries:

```
     4   stream_count
     per stream:
       16  stream_uuid
       4   entry_count
       per entry (one per DATA frame):
         8   back_offset    u64, bytes backwards from this INDEX frame's start
         8   first_axis     i64 ticks or f64, per the stream's axis_kind
         4   record_count   u32
         4   run_id         u32
```

Offsets are **relative to the INDEX frame's own position**, counted backwards, not
absolute file positions. An absolute offset is wrong the instant the file is
appended to another one (§6.6); a self-relative offset stays correct wherever the
file lands. The reader knows where the INDEX frame is, because it just read it.

Purely an accelerator — a reader MUST be able to rebuild it by scanning, and MUST
NOT trust it over the frames themselves.

An index that disagrees with the data is a corrupt index, not corrupt data. This
inversion of authority is what lets rule 2 hold: the file is correct without it.
It also means **concatenation may simply discard both indexes** (§6.6): the result
is a valid, unindexed file, and re-indexing is a scan a reader can do at any time.

## 10. Deliberate omissions

Things MDF4 has that this draft rejects, and why:

- **Unsorted records** — MDF4 lets records from different groups interleave in one
  block, tagged with a record ID, requiring a demux pass. It exists because writers
  couldn't buffer per group. Buffer per group. A logger that cannot afford one
  block of RAM per stream can emit single-record DATA frames instead, which costs
  12 bytes and no reader complexity.
- **Data groups** — a layer of indirection above channel groups with no meaning of
  its own. Streams are enough.
- **Linked lists of blocks** — replaced by sequence. Order is the list.
- **XML metadata** — replaced by key/value. An XML parser is a large dependency
  and a large attack surface for what is invariably used as a flat dict.
- **The algebraic conversion** — see §7.
- **Separate signal-data (SD) blocks for variable-length channels** — replaced by
  record tails.

## 11. Mapping: SPICE raw files

How an LTspice / ngspice `.raw` file maps onto this model. This is the check that
§5's axis generalisation and §6.5's runs actually earn their keep.

| SPICE raw | Logb |
|-----------|-----|
| `Title:`, `Date:`, `Command:` | file META keys |
| `Plotname: Transient Analysis` | stream META `sim.analysis` = `transient` \| `ac` \| `dc` \| `noise` \| `op` \| `tf` |
| `Flags: real` | `float` fields |
| `Flags: complex` | `complex` fields (§6.3) |
| `Flags: double` | field `bit_width` 64 vs 32 — per field, so the mixed case is free |
| `Flags: fastaccess` | `filter=transpose` on the DATA frame (§8) |
| `Flags: log` | `axis_mode=2`, implicit log (§5.3) — `ratio = 10^(1/points_per_decade)`, zero bytes per record. An importer may also write the axis explicitly, which is what LTspice hands it |
| `Flags: stepped` | one RUN frame per step (§6.5) |
| `No. Variables:` | `field_count` |
| `No. Points:` | not needed — frames are self-delimiting |
| `Offset:` | folded into `axis_base` |
| simulator timestep resolution | `axis_exp` — ngspice's internal time is `double` seconds; an importer picks the exponent that holds the run exactly (§5.1) |
| `Variables:` block, first variable | the axis: `time`→`axis_kind=time`, `frequency`→`axis_kind=frequency`, a swept source→`axis_kind=other` with `axis_unit` |
| `Variables:` block, rest | fields; the type column (`voltage`, `device_current`) → `unit` plus field META |
| the netlist | ATTACH frame |
| the run boundaries | **explicit** (§6.5), not inferred |

Notes on the quirks, which are the importer's problem and not the format's:

- The axis variable is always `f64` even when `Flags: double` is absent and every
  other variable is `f32`. Per-field `bit_width` expresses this directly, so it
  stops being a special case.
- LTspice stores the sign bit of the time axis as a marker, requiring `abs()` on
  read. An importer normalises this away; nothing about it survives into Logb.
- `Flags: compressed` is LTspice's own scheme, which `rveen/ltspice` rejects
  outright. It maps to `codec=zstd` on write and is a decode problem on import.

**Streaming is the bonus.** ngspice currently writes the raw file at the end of a
run, because the header states `No. Points:` up front and so cannot be written
until the count is known. Under Logb a simulator emits DATA frames as it solves, and
a long transient analysis becomes watchable while it runs. That falls out of rule 1
without being designed for.

## 12. Open questions

1. ~~**Name.**~~ **Resolved: Logb**, extension `.logb`, magic
   `89 4C 4F 47 42 0D 0A 1A`, sync token `LOGBSYNC`.

   The three-letter space this format would naturally have wanted is owned by the
   incumbents it means to replace: `BLF` is Vector's proprietary CAN log, `AXL` is
   ETAS's, and the near misses are worse than the hits — *Cadence* would suit a
   format built on periodic sampling, and it is an EDA giant. Every collision
   encountered came from a name that already meant something to somebody.

   `.logb` sidesteps that space rather than competing in it. `.log` is the most
   widely recognised extension there is, and appending `b` for the binary variant
   is an established move: `.xlsb` to `.xlsx`, `jsonb` to JSON, `.stlb` to ASCII
   STL. The name is therefore not coined and not an acronym — it is a description
   a reader can parse on sight, which is the one thing a coined word cannot do.
   `.logb` itself is unclaimed; `.dlog` was not (Keysight's power-analyser data
   log, binary, in this exact domain) and `.blf` least of all.

   Two costs are accepted knowingly. The `+b` convention implies a text twin, and
   there is no `.log` that this is the binary form of. And `logb` echoes Go's
   `math.Logb`, the binary-exponent function — no import conflict, since both are
   package-qualified, but it is the first hit when searching the name. Both were
   judged cheaper than opacity: this format is written by embedded loggers, so
   *log* is what its writers already call the thing.

   The name, the extension, and the magic are three separate decisions that only
   look like one: `gzip` is `.gz` with magic `1f 8b` and no letters at all. For
   comparison, the MDF4 writer in `golib/formats/mdf` writes `.mf4`.
2. ~~**Schema identity across files.**~~ **Resolved:** split routing from identity
   (§6.6). `stream_id` stays a `u16` but is segment-scoped, so concatenation is
   byte-concatenation and collisions cannot occur; `stream_uuid` in the SCHEMA
   frame decides what merges. The "almost" is gone.
3. ~~**Encryption / signing.**~~ **Resolved:** split them. **Encryption is out of
   scope permanently** — this is an open format, and confidentiality belongs to
   the layer that stores or moves the file, where it is solved once for every file
   type. **Signing is reserved, not specified**: frame type `0x50` (§3.3), which
   would cover the preceding segment. Per-segment is the only shape that works,
   because a detached signature is void after exactly the two operations this
   format promises to survive — truncation and concatenation.
4. ~~**Sub-nanosecond time.**~~ **Resolved:** `axis_exp` (§5.1). Time is an
   integer count of a declared tick, defaulting to nanoseconds and reaching
   femtoseconds without a second layout or a loss of exactness.
5. ~~**Log-spaced implicit axis.**~~ **Resolved:** added as `axis_mode = 2`
   (§5.3). An AC decade sweep is uniform in log space and now costs zero bytes per
   record like any other implicit axis. It was cheap to add before 0.1 and would
   have been a breaking change after.
6. ~~**Is `run_id` per DATA frame the right granularity?**~~ **Resolved:** keep the
   granularity, forbid the shuffle (§6.5). `run_id` stays per DATA frame — it is
   four bytes and no complexity for a logger that never uses it — but a stream's
   runs MUST be contiguous within a segment. The interleaving that per-frame
   granularity technically permitted is a file no reader wants; the multi-corner
   simulator that wants to interleave buffers per run, exactly as §10 tells MDF4's
   unsorted-record writers to.
7. ~~**Compression dictionary sharing** across segments.~~ **Resolved: dropped.**
   Not as a tradeoff against rule 3 — the feature dissolves under its own
   definition. Sharing a dictionary across segments asks a self-contained unit to
   depend on something outside itself, which is not a cost to weigh but a
   contradiction with what a segment is (§3); and a dictionary restated per
   segment is not shared, it is just per-segment compression state.

   The rule-3-safe reading of the idea — a dictionary declared after each SYNC
   frame, letting the DATA frames *within* one segment share statistics — is
   coherent and still loses. The cheaper answer to "my frames do not share
   statistics" is to put more records in a frame, which is already the writer's
   lever and costs no new machinery. A dictionary needs a training pass over a
   corpus and needs to stay resident in RAM, neither of which the embedded logger
   this format is built around can afford; it is an analysis-tool feature in a
   logger-first format. And dictionaries pay off most on payloads small enough
   that the 12-byte frame header and the per-segment schema restatement already
   dominate — it optimises the wrong end.

   A dictionary *referenced by id* rather than carried in the file is rejected
   outright and permanently: that is rule 4. A file whose bytes cannot be decoded
   without an artefact that must still exist in 2050 is not self-contained,
   whatever it does for the ratio.

   Dropping it costs nothing, which is why it is dropped rather than left open. A
   dictionary needs a new `codec` id, and a v0.1 reader meeting one fails loudly
   and specifically (§8). Anyone with a benchmark showing it beats a bigger batch
   can add it in a later version without breaking a single existing file or
   reader. If nobody produces that benchmark, that is the answer.
8. ~~**Is `record_bits` as `u64` overkill?**~~ **Resolved:** yes. It is a `u32`
   (§6.1) — a 512 MiB record, which is still far past anything real.
9. ~~**Bit numbering for unaligned big-endian fields.**~~ **Resolved: bit
   numbering follows the byte order** (§6.2). `bit_offset` names the field's first
   bit — counting from the LSB of its byte for little-endian, from the MSB for
   big-endian — and the field runs upward from there. One rule, both orders, no
   alignment case, and the big-endian half is exactly DBC Motorola.

   The draft's option (b), forbidding unaligned big-endian and making importers
   normalise, was **wrong on its facts** and is worth recording as such: it
   claimed normalisation "costs them a shift". It does not. A Motorola signal has
   no little-endian bit-slice equivalent at all unless it fits inside one byte —
   not 8 of 10 cases, but every multi-byte signal, including the byte-aligned
   16-bit ones the draft already accepted. There was no shift to be had. Option
   (c), an explicit `bit_numbering` field, would have admitted the ambiguity as
   permanent and made every reader implement both numberings anyway.

   The reason (a) is not the compromise it looks like: each byte order's fields
   are contiguous *in that order's own numbering*, so stating the rule per byte
   order removes the sawtooth rather than encoding it. What remains is about
   twenty lines per reader, which is the honest price. The alternative was not a
   cheaper format but one that cannot describe the signals inside a CAN frame —
   and then reading them would need a DBC parser, which is exactly the external
   schema dependency rule 4 exists to forbid.

   It was settled with an executable oracle rather than with real DBC files, which
   turned out to be the stronger test: agreement with Vector's reference algorithm
   across 465,600 cases. The conformance vectors in §6.2 are what keep it settled.
