# Rationale: the SYNC frame and the segment

Why the format is cut into segments, and why the boundary marker is these 32
bytes.

The standard this document is held to is stated in [file-header.md](file-header.md):
every piece of complexity has to pay for itself, and anything that cannot be
justified in writing is recorded as unjustified rather than defended.

## What it is

```
frame_type 0x01, stream_id 0, payload 32 bytes:

+0   16   sync_pattern    4C 4F 47 42 53 59 4E 43 A7 3E 91 D2 5C 68 0B F4
+16   8   segment_seq     u64, monotonic from 0
+24   8   wall_time_ns    i64, wall clock at segment start, 0 if unknown
```

And the grammar it opens:

```
segment := sync-frame schema-frame+ run-frame* (meta | attach | data)*
```

## Why segments exist at all

Rule 3 — *a reader handed the middle of a file, with no access to the start, can
resynchronise and decode records with full schema*.

That rule is the entire justification. Without it there is no reason to ever
restate a schema, and the format would state each one once and reference it by id
forever, which is smaller and simpler and fails the moment anyone hands you a
partial file.

Partial files are not an edge case in this domain. They are:

- a recording that lost power, where the tail is gone;
- an SD card recovered with holes in it;
- a stream tapped live, joined at an arbitrary moment;
- a file split by a tool that does not know the format;
- the second half of a `cat` of two recordings.

In every one of those, a reader is holding bytes whose beginning it does not
have. A format that stores schema once has, at that point, an undecodable blob —
the bytes are all there and their meaning is not. **Repeating the schema is the
price of the data being self-describing at every cut face**, and a segment is the
unit that price is paid in.

So the segment is a *self-contained decode unit*: everything needed to decode its
DATA frames appears between its SYNC frame and the next one. Nothing before it is
required, which is the same statement as: every binding expires here.

### Why bindings expire, rather than accumulate

`reader.go:310` throws away the whole `stream_id` and `run_id` map on every SYNC
frame. That is deliberate and it is what makes the property true rather than
approximately true. If ids carried across segments, a reader that joined at
segment 40 would decode until it met an id first bound in segment 3 — and then
fail, having produced records for a while and given the impression it was fine.
Expiry converts "usually decodable" into "decodable", which is the only version
of rule 3 worth having.

It also makes concatenation free. File B's `stream_id = 1` is not a collision
with file A's; it is a new binding, which is what a SYNC frame *means*. That is
argued in SPEC.md §6.6 and belongs to the concatenation rationale.

## Why a fixed pattern, and why it is a frame

### The scan problem

A length-prefixed format can walk forward from a known boundary and cannot find
one. Handed an arbitrary offset, a reader cannot scan for a frame header, because
every four bytes are a plausible `payload_len` and `0x01` is a common byte. There
is no structure in an 8-byte header rare enough to search for.

So recovery needs a token that is *improbable in data*. Sixteen bytes of it, and
then a verification step: `Resync` (`reader.go:616`) finds the pattern, backs up
exactly 8 bytes to where the frame header must be, and requires that
`frame_type == 0x01` and the CRC over the whole frame matches. Pattern to
narrow, CRC to decide.

### Why 16 bytes, and why half of them are letters

Eight bytes of the pattern are `LOGBSYNC` and eight are random
(`logb.go:36`). The split is doing two different jobs:

- **The letters are for humans.** The same argument as the four letters in the
  file magic: `xxd` on a broken recording shows you segment boundaries without
  a tool, and `grep -b LOGBSYNC` is a usable forensic instrument.
- **The random tail is the entropy.** It needs no NUL padding and no structure;
  its only job is to be a byte sequence nothing else produces.

Sixteen rather than eight because the pattern must be improbable in *structured*
data, not in random data. Random-collision arithmetic on 8 bytes already looks
safe, but files are not random: they contain ASCII, repeated calibration
constants, zero-fill, and other files (see the ATTACH case below). Doubling the
token costs 8 bytes per segment — nothing, at one segment per megabyte — and
removes an entire class of argument about whether the data could produce it.

### Why the pattern sits inside a normal frame

It would have been possible to make the pattern *be* the segment header — a
special 16-byte structure the sequential reader recognises. That was rejected
because it would put a special case in the hot path: the frame walk would have to
test for it on every iteration, and "every frame has one shape" would become
"every frame has one shape, except".

Keeping SYNC an ordinary frame means the sequential reader has **zero** knowledge
of the pattern. It reads a frame header, sees type `0x01`, and clears its maps.
Only `Resync` — the recovery path, called explicitly — knows the pattern exists.
The cost of framing the pattern normally is 12 bytes per segment; what it buys is
that the fast path and the recovery path share no code and no assumptions.

The backing-up-8-bytes trick is what ties the two together, and it works only
because the frame header is fixed-width. That is one of the reasons `payload_len`
is not a varint, argued in [frame.md](frame.md).

## The header argument, one level down

A SYNC frame is written *before its segment exists*. The writer at that moment
knows no more about the segment than it knew about the file when it wrote the
file header: not how many records, not which streams will actually produce data,
not whether the segment will end cleanly or in a power cut.

This is the same constraint as [file-header.md](file-header.md), recursively
applied, and it produces the same shape of answer. Only two things are knowable
when a segment begins — **which segment this is** and **what time it is now** —
and those are exactly the two fields present.

The parallel is not a coincidence. It is what rule 1 looks like at every scale:
any structure that opens something can only describe what is true before that
thing happens.

## Decision by decision

### `segment_seq` — u64, monotonic from 0

What it buys is only visible in damaged or partial files, which is the right test,
because intact files never need it — a sequential reader that has every segment
also has them in order.

- **A recovered card with holes**: two fragments with `seq` 12 and 47 tell you 34
  segments are missing. Without it you know only that something is.
- **A concatenated file**: `seq` resets to 0 at the join, which is how a tool
  identifies where one recording ended and the next began — information that
  survives even when both files' END frames were lost to truncation.
- **After `Resync`**: you know *which* segment you landed in, not merely that you
  landed.

`u64` where `u32` would last 136 years at a segment per second, because the field
is written once per segment and four bytes there are not worth the arithmetic of
justifying `u32`.

### `wall_time_ns` — i64, per segment, 0 if unknown

**Why per segment rather than once in the file header.** Because a logger's clock
is frequently wrong at t=0 and right later. An embedded recorder without an RTC
boots at epoch zero and acquires GPS or NTP minutes in. A per-segment wall clock
lets the truth land as soon as it is known, without patching anything and without
rule 1 being violated. A file-header timestamp would force the writer either to
lie or to seek back — which is precisely the MDF4 failure described in SPEC.md
§5.2.

**Why it is not an axis value.** It is a *coarse seek hint* — when the segment was
written — and explicitly not what the streams' axes mean (`writer.go:106`). The
axis story is `axis_base`, `axis_exp`, and the `time.anchor` META key; this field
answers "roughly when was this part of the recording made", which is what a
seek-by-date tool needs and nothing more. Keeping the two separate is why a
`monotonic` file can carry useful wall-clock hints without claiming its records
are dated.

**Signed, and `0` means unknown.** `i64` rather than `u64` so simulation and
historical data can sit before 1970 without a special encoding. Zero as the
sentinel is a magic value, which is mildly impure — but it means midnight
1970-01-01 UTC, and no real measurement claims that instant. The alternative is a
flag bit, and there is no flag byte in this payload worth introducing for it.

The range limit is real and worth stating: `i64` nanoseconds runs out in 2262.

### `schema-frame+` is mandatory, `run-frame*` is not

A segment must restate every schema its DATA frames use; it need not restate runs
it does not use. The asymmetry is just rule 3 applied honestly — a schema is
required to decode a record at all, so a segment without it is not self-contained;
a run is a parameter set attached to records that already decode, so a missing
RUN frame degrades interpretation rather than preventing it.

### Segment period is writer policy, deliberately unspecified

Every N megabytes or M seconds, and the spec declines to say which. The trade is
recovery granularity against schema-restatement overhead, and it is genuinely
application-specific: a crash-test recorder wants small segments and will pay for
them; a 200-channel bench log with large schemas wants the opposite. Specifying a
number would be inventing a default with no basis, and a conforming reader does
not care.

## What was deliberately kept out

| Not in the SYNC payload | Where it lives | Why not here |
|---|---|---|
| The schemas themselves | SCHEMA frames following it | Streams vary in number and size; folding them in would make one variable-length mega-frame and destroy the fixed 32-byte shape `Resync` relies on |
| Segment length / record counts | Nowhere; derived by scanning, or INDEX | Unknowable when the segment opens — the recursive t=0 argument above |
| A pointer to the *previous* segment | Nothing | See below |
| File metadata | META frames | Not all known at segment start, and it is file-scoped, not segment-scoped |
| Schema hash or fingerprint | `stream_uuid` in each SCHEMA payload | Identity is per stream, not per segment |

**The near-miss worth recording: a back-pointer to the previous segment would be
legal.** Rule 1 forbids forward references, not backward ones, and eight bytes
per segment would give a reader a linked list it could walk in reverse — read the
last segment, jump backwards, no scanning.

It was still rejected, on two grounds. First, INDEX already provides random
access with backward offsets, so the chain would be a *second* index that must
agree with the first, and two structures that can disagree about the same fact is
a bug surface with no upside. Second, the chain is only usable by a reader that
has the end of the file — and a reader that has the end has the INDEX. It buys
nothing that is not already bought, in the only case where it works.

This one is worth keeping written down precisely because rule 1 does not reject
it. It was rejected by "does it earn its bytes", which is the harder test.

## Costs accepted

**Schema restatement.** Every segment repeats every active schema, which for a
200-channel stream with long names, units, descriptions and conversions is not
trivial. This is the direct cost of rule 3 and it is bounded by writer policy:
segment period is the knob that trades this against recovery granularity. It is
the format's largest deliberate redundancy and it is the one it exists for.

**`Resync` is a linear scan.** Finding a boundary in a file whose framing you have
lost costs a `memmem` over the region, plus one CRC per candidate hit. There is
no index to consult, because a reader in this position may not have the end of
the file either. Recovery is O(n) by construction.

**A Logb file inside an ATTACH frame contains real sync patterns.** This is a
genuine false positive, not a probabilistic one: the embedded file's SYNC frames
are byte-identical to real ones, complete with valid frame headers and correct
CRCs, so the verification step cannot reject them. A `Resync` that lands inside
an attachment will decode the *attached* file's segment and return valid records
from it.

The damage is bounded — the output is well-formed Logb data, just from the
embedded file rather than the container — but the semantics are murky, and a tool
that resyncs into a file with attachments cannot tell which of the two it is
reading. Recorded here as a known hole rather than solved: the fix would be
escaping or a per-file nonce in the pattern, and both cost more than the case is
worth.

## Not yet justified

**Both non-pattern fields are dead in the reference reader.**

`segment_seq` is parsed into `r.seq` (`reader.go:314`) and then never read —
there is no continuity check, no gap detection, and no exported accessor on
`Reader`. `wall_time_ns` is not even stored: `reader.go:315` discards it with
`_ = d.i64()` and a comment noting it is a seek hint.

So 16 of the SYNC payload's 32 bytes currently do nothing in this
implementation. The uses argued for above — gap detection, join detection,
seek-by-date — are all real and all *unimplemented*, which is a different claim
from "unjustified" but not by much under this directory's standard. Two honest
options:

1. Implement them. Gap detection is a comparison; seek-by-date is the coarse
   index `wall_time_ns` was put there to be. If the fields are worth their bytes,
   something should consult them.
2. Or record that they exist for *external* tooling — `grep`, a forensic dumper,
   a future indexer — and that the reference reader is not their consumer. That
   is a defensible position, but it should be stated rather than left implicit,
   because as the code stands a maintainer cannot tell the difference between
   "reserved for tools" and "forgotten".

Until one of those happens, this is the same finding as `version_minor` in
[file-header.md](file-header.md) and `flags` in [frame.md](frame.md): a field
that is written faithfully and consulted by nothing. Three instances make it a
pattern worth naming — the format is more willing to add a field than to add the
code that depends on one.
