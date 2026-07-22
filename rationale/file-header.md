# Rationale: the file header

Why the first 16 bytes are what they are.

> **Why this directory exists.** Every piece of complexity in this format has to
> pay for itself. A rationale document is the receipt. If a field, a flag, or a
> rule cannot be justified in writing — what it buys, what it costs, and what
> breaks without it — it is not justified, and it should come out of the format
> rather than stay in on the grounds that it is already implemented. These
> documents are therefore written to be *falsifiable*: where something does not
> currently earn its place, that is recorded too, at the bottom, rather than
> argued away.

## What it is

```
+0   8   magic       \x89 L O G B \r \n \x1a
+8   2   version_major = 0
+10  2   version_minor = 1
+12  4   crc32c of bytes 0..11
```

Sixteen bytes, written once, before anything else exists.

## The constraint that decides almost everything

The header is emitted at t=0, when the writer knows nothing. It has no stream
list yet, no record count, no duration, no idea whether the session will end
cleanly or with a power cut. Under rule 1 — *nothing points forward, a writer
never seeks back to patch a field it already emitted* — that ignorance is
permanent. Whatever is not knowable at t=0 can never appear here.

That single fact explains the header's size. It is not small because small is
elegant; it is small because it is the intersection of "true before any data
exists" and "still true at end of file". Almost nothing qualifies.

Contrast MDF4, whose header block carries counts and absolute links into
structures written much later. That is a coherent design, and it is exactly why
an MDF4 writer needs `io.WriteSeeker` while `logb.Writer` needs only
`io.Writer` (`writer.go:17`). A pipe, a socket, a serial line, and a file being
tailed are all valid Logb sinks. That capability is bought here, in the header,
by refusing to put anything in it.

## Decision by decision

### That there is a header at all

It has to be argued for, because under rule 3 the header is not needed to decode
anything. `Resync` (`reader.go:616`) recovers a full decode — schema included —
from the middle of a file that has no header in sight. So the format works
without it.

What it buys:

- **Identification.** `file(1)`, content sniffers, and a human with `xxd` can all
  tell what they are holding, from the first eight bytes, without parsing.
- **A cheap, early, correct error.** Handed a JPEG, `NewReader` returns
  `ErrBadMagic` immediately rather than scanning megabytes for a sync pattern
  that will never appear.

What it costs is stated plainly under [Costs accepted](#costs-accepted). This is
the format's one unrepeated, non-self-locating structure, and that is a real
violation of the varve principle, accepted for the two benefits above.

### The magic, and why it is PNG's

PNG's 1996 signature solved this problem correctly and the failure modes it
guards against have not gone away — files still cross S3, CI artifacts, email,
and Windows shares. Inventing a fresh eight bytes would cost the same eight bytes
and detect strictly less. So each byte here has a job, and they are PNG's jobs:

| Byte | Job |
|------|-----|
| `\x89` | High bit set. Catches a 7-bit-clean transport that stripped the eighth bit, and stops any content sniffer reading the file as text. |
| `LOGB` | Human-readable in a hex dump. Signature matching for `file(1)`. |
| `\r\n` | Catches line-ending translation: a CRLF→LF transport eats the `\r` and the compare fails. |
| `\x1a` | DOS end-of-file. `type file.logb` stops here instead of spraying binary at a terminal. |

**The one deliberate divergence:** PNG spends a ninth byte on a trailing `\n`, to
catch the LF→CRLF direction its `\r\n` pair does not. Logb spends that byte on
the fourth letter of the name instead. The loss is smaller than it looks: under
LF→CRLF translation the `\r\n` inside the magic becomes `\r\r\n`, so the compare
fails anyway. The two guards overlap on the case the ninth byte was buying, and
dropping it keeps the header at 16 bytes with a four-letter name inside it.

This is the trade recorded honestly: a *sliver* of detection strength — the LF→CRLF
case where the file contains no other `\r` to be doubled — traded for a name that
reads in a hex dump.

### Two version fields rather than one

The split is not decoration; the two halves name two different and mandatory
reader behaviours:

- **`version_major` — reject.** An unknown major means the frame grammar itself
  may have changed. `NewReader` returns `ErrBadVersion` (`reader.go:272`) and
  reads nothing. Guessing would be worse than failing.
- **`version_minor` — accept, and skip what you do not know.** Additive change
  only. Unknown frame types are skipped by `payload_len`, unknown codecs reject
  their own frame, an unknown `axis_mode` skips its stream.

Encoding these as one number would force every reader to know the split rule.
Encoding them as feature bits is impossible here: a capability bitmap describes
content, and content is not known at t=0 (see the constraint above). This is the
clean case where the header field is the *only* place the information can live,
because it is a property of the writer, not of the data.

Two bytes each rather than one is over-provisioned, and is admitted as such below.

### A CRC over a 12-byte header

Every frame carries its own CRC, so this one needs a separate argument. It has
two:

1. **The header has no framing to fall back on.** Everywhere else, corruption is
   caught structurally — a bad `payload_len` runs past end of input, a bad frame
   fails its own CRC, and `Resync` re-establishes the stream. The header has no
   length field, no successor to disagree with, and no sync pattern. A flipped
   bit in `version_major` produces a confident wrong answer: a good file rejected
   as a future version. The CRC turns that into `ErrCorrupt`, which is true.
2. **One checksum routine, applied uniformly.** Under rule 4 a conforming reader
   is ~1000 lines with no dependencies. Having the header checked by the same
   `crc32c` as everything else means there is exactly one checksum implementation
   in a reader, not one plus a special case.

Cost: four bytes, once per file. Even at one file per second this is noise.

`crc32c` is Castagnoli (0x1EDC6F41) rather than the zlib polynomial because it
has a hardware instruction on x86-64 (SSE4.2) and ARMv8, which matters when the
same routine runs over every DATA frame on an embedded logger.

### Sixteen bytes

The four fields sum to exactly 8+2+2+4. There is no padding and no alignment
argument — frames are variable-length, so nothing after offset 16 is aligned
anyway, and a claim that 16 helps `mmap` readers would be false. The number is
the sum, and the sum is round because the version fields were sized to make it
round.

## What was deliberately kept out

Each of these was considered and belongs elsewhere. The pattern is the same every
time: the header cannot hold it without a seek-back, and something later can hold
it without one.

| Not in the header | Where it lives | Why not here |
|---|---|---|
| File metadata (device, operator, units) | META frames (0x11) | Not all known at t=0; a META frame can be emitted the moment it is |
| Stream list, schemas | SCHEMA frames, restated per segment | Rule 3: a reader that starts mid-file must get these anyway, so putting them here would duplicate, not replace |
| Index offset | INDEX frame at the end, offsets counted *backwards* | A forward pointer is rule 1; a backward one needs no patching |
| Creation time | `wall_time_ns` in each SYNC frame | Per-segment is strictly more informative, and survives truncation and concatenation |
| Record count, duration | Derived by scanning, or read from INDEX | Unknowable at t=0 by definition |
| Compression, feature flags | Per-DATA-frame `codec` and `filter` | A writer may change codec mid-file; a whole-file flag would have to be patched |
| Cleanly-closed marker | END frame (0x40) | Unknowable at t=0 — and a header flag would be *wrong* precisely in the power-loss case it was meant to describe |

The last row is the sharpest. A "file is complete" bit in the header is the
canonical mistake: it can only be set by seeking back, so a file that lost power
carries a header that lies. END frames state the same thing as a fact about the
past, at a position where it was true when written.

## Costs accepted

**Concatenation is not quite `cat`.** Two Logb files join by appending the
second's bytes *minus its 16-byte header* (§6.6). Everything else in the format
concatenates blind — `stream_id` is segment-scoped, SYNC rebinds it, INDEX
offsets are backwards — and the header is the sole reason the operation needs a
`tail -c +17` rather than nothing at all.

This is a real cost, and it is the price of identification. It is bounded: the
strip is trivial, needs no parsing, and cannot corrupt anything. A reader that
meets an unstripped header mid-file is not lost either — the bytes fail as a
frame, and `Resync` picks up at the next sync pattern — but the result is a file
that only a resyncing reader can fully read, so writers strip.

**The header is a single point of failure for identification.** Lose the first 16
bytes and `NewReader` fails, even though `Resync` can still decode the whole
file. This is by design — recovery is `Resync`'s job, not the header's — but it
should be said out loud that the format has one structure whose loss changes
which API you must call.

## Not yet justified

Recorded here rather than defended, per the standard at the top of this file.

**`version_minor` is currently write-only.** `writer.go:63` writes it;
`NewReader` (`reader.go:271`) reads `major` and never touches bytes 10–11. The
comment there — "an unknown higher minor is fine" — is accurate precisely
*because* nothing consults the field: minor-version tolerance is delivered
entirely by per-frame skipping, which would work identically if the field did not
exist. (`cmd/logbdump` prints `logb.VersionMinor`, the library's constant, not the
file's.)

So the field buys diagnostics only: a human or a bug report can see which writer
version produced a file. That may well be worth two bytes — but it is a *different*
justification from the one the field's name implies, and until a reader actually
uses it, the honest statement is that Logb's compatibility story does not depend
on it.

**The version fields are over-sized.** `u8` each would cover 255 major and 255
minor versions, which for a format promising to be readable in 2050 is not the
binding constraint. Four bytes are spent where two would do; the extra two are
what round the header to 16. "It makes the number nicer" is a weak rationale and
is labelled as such.

**A major-version bump is close to an admission of failure.** Rule 4 promises a
reader still works in 2050. If `version_major` ever becomes 1, every existing
reader correctly refuses every new file, and the format has forked. The field is
therefore best understood not as a planned evolution path but as a *fuse*: it
exists so that a hypothetical incompatible successor fails loudly instead of
silently mis-decoding. Worth two bytes on those terms, and only those.
