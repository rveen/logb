# Rationale: the frame

Why the format is framed at all, and why the frame header is these eight bytes.

The standard this document is held to is stated in [file-header.md](file-header.md):
every piece of complexity has to pay for itself, and anything that cannot be
justified in writing is recorded as unjustified rather than defended.

## What it is

```
+0    4   payload_len   u32, bytes of payload only
+4    1   frame_type    u8
+5    1   flags         u8
+6    2   stream_id     u16, 0 if not stream-scoped
+8    n   payload
+8+n  4   crc32c        over the 8-byte header and the payload
```

Twelve bytes of overhead per frame: eight in front, four behind.

## Why frames at all

This is the load-bearing decision. Everything else in this document is
consequence.

A measurement file is produced by a writer that does not know the future and
consumed by a reader that may not have the past. Framing — chopping the byte
stream into self-delimiting, self-checking units — is what makes those two
positions survivable. Three properties fall out of it, and the format's first
three rules are exactly those properties:

**1. You can append without knowing what comes next.** A frame is complete the
moment it is written. Nothing already on disk needs revisiting when the next one
arrives, so a writer needs no seek, and therefore no seekable sink: a pipe, a
socket, a serial line, an append-only object store, a file someone is tailing.
This is rule 1, and it is why `logb.Writer` takes an `io.Writer` where an MDF4
writer needs `io.WriteSeeker` (`writer.go:17`).

**2. You can cut anywhere and lose only the cut.** Power fails mid-write. The
last frame is incomplete — its CRC will not match, or its `payload_len` runs past
end of input — so a reader stops there and everything before it is intact and
readable. Damage is *bounded by the frame*, not by the file. There is no repair
tool because there is nothing to repair: truncation is a supported operation, not
a corruption. This is rule 2.

**3. You can skip what you do not understand.** Because a frame states its own
length before its content, a reader can step over a frame whose meaning is
completely unknown to it. That is what makes the format extensible with no
registry, no negotiation, and no version handshake — a reader from 2026 walks a
file written in 2040 by stepping over the frame types it has never heard of. It
is the *only* extension mechanism, and it is enough.

### What the alternatives cost

| Design | How it fails here |
|---|---|
| **Whole-file structure with absolute links** (MDF4) | Blocks reference each other by offset, so offsets must be patched once their targets exist. Needs a seekable sink, and a truncated file is a graph with dangling edges — hence repair tools. Fails rules 1 and 2. |
| **One frame per record** | Correct, but 12 bytes of overhead on an 8-byte CAN payload is 150% overhead, and one CRC per record on an embedded logger is real CPU. Fails rule 6's spirit by making the per-record cost dominant. |
| **Sentinel-delimited records** (scan for a terminator) | Requires escaping every occurrence of the sentinel inside the payload, so the bytes on disk are no longer the bytes the sensor produced — a direct violation of rule 5 — and finding a boundary becomes a linear scan instead of an add. |
| **No framing, external sidecar index** | The sidecar is void the moment the file is truncated or concatenated, which are the two operations the format promises to survive. |

Frames are the middle position: **batches**, not records and not files. Big
enough that 12 bytes is noise, small enough that losing one to a power cut costs
little.

### Framing and resynchronisation are two different mechanisms

A length prefix lets you walk *forward* from a known boundary. It is useless if
you do not have a boundary — handed the middle of a file, a reader cannot scan
for a length, because any four bytes are a plausible length.

So the format carries both: `payload_len` to walk forward cheaply, and the 16-byte
SYNC pattern to re-enter a stream when the walk is lost (`Resync`,
`reader.go:616`). Each covers the other's blind spot, which is why neither is
redundant. The frame header is the fast path; the sync pattern is the recovery
path.

## The test every header field must pass

The frame header is read by a reader that does **not yet know what the frame
is**. So it may contain exactly one class of information: what you need in order
to decide whether to skip, and how far. Anything else belongs in the payload,
where only a reader that understands the type will pay for it.

| Field | Needed to skip a frame you don't understand? |
|---|---|
| `payload_len` | Yes — it *is* the skip distance |
| `frame_type` | Yes — it is how you decide to skip |
| `stream_id` | Yes — it is how you decide to skip a type you *do* understand (rule 6) |
| `crc32c` | Not to skip, but to know whether to believe any of it |
| `flags` | **No.** See [Not yet justified](#not-yet-justified) |

## Decision by decision

### `payload_len` first, and a `u32`

**First**, because the skip decision must be answerable from the fewest possible
bytes. A reader that wants nothing from this frame reads four bytes and seeks.
Putting the type first would read better but would mean every skip touches five.

**`u32`**, not `u64`: four extra bytes on every frame to describe batches that
cannot be that large anyway. A frame is assembled whole in memory before it is
written (see [Batch sizing](../SPEC.md#81-batch-sizing)), so a frame that does not
fit in RAM is not writable in the first place. `u32` also bounds what a *corrupt*
length can demand — a reader allocates `payload_len` bytes before it can check
the CRC, and 4 GiB is a bad day where 16 EiB is an immediate kill.

**Not a varint**, though it would save two or three bytes on small frames: a
variable-width length makes the header itself variable-width, so a reader can no
longer read a fixed 8 bytes and know where it stands, and `Resync`'s trick of
backing up exactly 8 bytes from the sync pattern stops working. Rule 4 — a reader
in ~1000 lines — is worth more than three bytes per batch.

**It counts the payload only**, excluding both the header and the CRC. The number
therefore means exactly one thing: how many bytes to hand the payload parser.
Total footprint is `12 + payload_len`, computed once in `Frame.Size`
(`reader.go:94`). Counting the whole frame instead would have been equally
self-consistent, but then every payload read would carry a `- 12`, and off-by-12
is a more attractive bug than it looks.

### `frame_type` — one byte

256 types where 9 are defined. `u8` rather than `u16` because the extension
mechanism is deliberately *cheap to use but rare to need*: new frame types are
the format's slowest-moving axis, and a design that expected hundreds of them
would be admitting that frames were the wrong abstraction. The spare byte also
keeps the header at 8.

### `stream_id` in the header, not the payload

This is the field that buys rule 6 — *adding a channel must not change the cost
of decoding an unrelated channel*. A tool plotting one signal out of two hundred
reads eight bytes per frame, compares one `u16`, and seeks past every batch it
does not want. It never decompresses, never de-transposes, never parses a record.
Had the stream identity lived in the DATA payload, that same tool would have to
decode every batch in the file to discover it wanted none of them.

**`u16`, and segment-scoped.** 65,536 concurrent streams per segment is far past
any real instrument, and the id is rebound at every SYNC frame, so it never has
to be globally unique. The identity that *is* global is `stream_uuid`, which
lives in the SCHEMA payload where it is read once per segment rather than in
every frame header. Putting a UUID in the header would cost 14 more bytes on
every batch to solve a problem that arises once per segment; that trade is
argued in SPEC.md §6.6 and belongs to the concatenation rationale, not this one.

**`0` when not stream-scoped**, so SYNC, ATTACH, RUN, INDEX and END need no
special case — they simply carry zero, and a reader filtering by stream never
matches them.

### `crc32c` at the tail, not the head

It cannot be anywhere else. A checksum over the payload is unknown until the
payload is complete, so a CRC in the header would have to be written as a
placeholder and patched afterwards — a seek-back, which rule 1 forbids outright.
The trailer is not a stylistic choice; it is the only position compatible with a
non-seekable writer.

Two consequences worth stating:

- **The CRC validates the length field by construction.** You can only find the
  trailer by trusting `payload_len` first. If the length is wrong, you compare
  the wrong four bytes, the check fails, and the read stops — which is the
  correct outcome. There is no separate header checksum because there is no way
  for a bad length to pass this test.
- **It covers the header too**, so a flipped bit in `frame_type` or `stream_id`
  is caught rather than silently routing a batch to the wrong stream.

### Twelve bytes, and why that is affordable

Only because frames are batches. At a thousand records per DATA frame, 12 bytes
is a rounding error; at one record per frame it would dominate a CAN trace. The
overhead number is not defensible on its own — it is defensible *given* batching,
and batching is in turn forced by `payload_len` and the trailing CRC, since both
require the payload to be complete before the first byte is written.

That circle is deliberate and it closes: the framing decision makes streaming
possible, and streaming with a length prefix makes batching mandatory, and
batching makes the framing overhead free.

## What was deliberately kept out

Every one of these was a candidate for the common header and belongs in the DATA
payload instead, for the same reason each time: it is meaningful to exactly one
frame type, and a field in the common header is paid for by *every* frame.

| Not in the frame header | Where it lives | Cost if it were here |
|---|---|---|
| Timestamp / `axis_base` | DATA payload, +0 | 8 bytes on every SYNC, SCHEMA, META and END frame — and its type depends on `axis_kind`, which the header cannot know |
| `record_count` | DATA payload, +8 | Meaningless for every non-DATA frame |
| `codec`, `filter` | DATA payload, +16 | Compression is a property of a batch, not of framing; META and SCHEMA frames are not compressed |
| `run_id` | DATA payload, +12 | Meaningful only where records exist |
| Sequence number | SYNC payload (`segment_seq`) | Per-frame sequencing would be 8 bytes to detect a gap that truncation cannot produce and corruption already fails on |

## Costs accepted

**A corrupt frame ends the read.** By design: a reader that meets a bad CRC stops
rather than guessing (`reader.go:561`). The cost is that damage in the middle of
a file makes everything after it unreachable *to a sequential reader* — recovery
requires calling `Resync`, which is a different entry point. The format bounds
the loss; it does not make sequential reading immune to it.

**Latency is quantised by batch size.** Nothing in a partially-filled frame is
visible to a reader, so a stream that produces a record every second and batches
a thousand is ten minutes behind. That is a writer-policy knob, documented in
SPEC.md §8.1, but it is a real consequence of framing rather than a tunable that
makes it go away.

**Twelve bytes are unavoidable even when unwanted.** A META frame carrying a
six-byte key/value pair still pays the full 12. Small frames are rare enough that
this has not been worth a compact form, and a second frame encoding would defeat
the "one shape, always" property that makes the reader small.

## Not yet justified

**The `flags` byte.** It is written as zero (`writer.go:327`), surfaced by the
reader as `Frame.Flags` (`reader.go:547`), and consulted by nothing. It fails the
test this document sets for the header — it carries no information a reader needs
in order to skip a frame it does not understand — and v0.1 defines no bit in it.

What it actually buys today is arithmetic: without it the header is 7 bytes,
`stream_id` lands at an odd offset, and payloads start at +7. "It rounds the
header to 8" is the same weak rationale that rounds the file header to 16, and it
is labelled the same way here.

There is a sharper problem than the byte itself. **No reader rejects a non-zero
`flags`**, so the field cannot safely carry a semantic change later: a v0.2
writer that sets bit 0 to mean something would be silently ignored by every v0.1
reader, which is precisely the failure mode the format avoids everywhere else
(an unknown `codec` rejects its frame; an unknown `axis_mode` skips its stream;
an unknown frame type is skipped by length). As specified, `flags` can only ever
hold hints that are safe to ignore. If that is the intent it should be written
down; if it is not, readers must be required to reject unknown bits, and that
requirement has to land before anything is written into the field.

**Implementation note, not a format defect:** `reader.go:551` allocates
`payload_len` bytes before the CRC can be checked, so a corrupt header can demand
up to 4 GiB. The format bounds this at `u32` on purpose (above), but the code
already has `maxAllocHint` guarding exactly this class of problem for `raw_size`
and does not apply the same reasoning here. Worth closing.
