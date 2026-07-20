# logbview

A browser-based viewer for Logb files, in the spirit of what asammdf's viewer is
for MDF. Open a file, pick signals from a tree, plot them, zoom in.

```
go run ./cmd/logbview ../testdata/can-example.logb
```

It indexes the file, starts a local HTTP server, and opens your browser at it.

## Why this is a separate module

The core library advertises a ~1000-line reader with near-zero dependencies
(SPEC.md rule 4), and everyone who `go get`s `github.com/rveen/logb` downloads
the whole module zip. The nested `go.mod` here keeps the embedded frontend
bundle out of the parent module's file set, so the core stays as small as it
claims to be. The server itself is Go standard library only.

## Status

Phases 1 to 3 are done, and the scale target is met. Measured on a 100 million
record fixture (600 million samples across six fields):

| | |
|---|---|
| First open | 24 s, with a progress bar |
| Subsequent opens | 7 ms, from the sidecar index |
| Resident memory | 22 MB indexing, 10 MB serving |
| Whole-file overview | 2 ms |
| Zoom to one second (100k samples) | 17 ms |

Memory is bounded by frames × fields, not by samples, so it barely moves with
file length.

What works today:

- Signal tree over streams, fields and runs, with `sparse`, `state` and `axis`
  badges.
- Numeric signals as min/max envelopes, so a one-sample spike survives
  decimation instead of being strided away.
- Categorical signals (enumerations under `value_to_text`, and bools) as state
  bands rather than lines.
- Drag to zoom; every pane shares one axis.
- File metadata, stream metadata, field metadata, and attachments.
- Truncated / closed-cleanly / unsupported status reported honestly.
- Streams that were declared but never wrote a record, listed rather than
  silently dropped.
- Random access: any DATA frame decodes on demand, including on a truncated
  file and across a concatenation join.
- Two-tier decimation: whole-file overviews from per-frame statistics, exact
  samples once zoomed in, with the chart saying which it is showing.
- A sidecar index cache that survives the file growing underneath it.
- The server comes up before indexing finishes, with progress over SSE.
- A record table over the same window the charts show, and CSV export.
- Event lanes: string and byte fields as marks with labels, thinning to a
  density profile when there are too many to draw.
- Run overlays: every run of a field on one pane, colour-coded and labelled by
  the parameter that was stepped.
- Non-time axes, including logarithmic ones.
- A frame-map inspector: the file's byte layout, drawn to scale.

## How random access works

The core reader is single-pass and has no seek API, and SPEC §9 forbids trusting
the file's own INDEX frame over the frames themselves. Rather than fork the
reader, `index/access.go` replays what it needs.

Rule 3 says a Logb file can be cut anywhere and still decode, because every
segment restates its schemas. The corollary is that a segment's preamble placed
in front of any of that segment's DATA frames is a byte stream the *unmodified*
reader decodes correctly:

```go
io.MultiReader(
    prefix,                                   // file header ‖ SYNC ‖ SCHEMA ‖ RUN…
    io.NewSectionReader(f, frame.Offset, n),  // any DATA frame, in any order
    …
)
```

Nothing in the decode path reaches forwards or backwards, and a DATA frame
carries its own `axis_base`, `record_count`, `run_id`, codec and filter. This
isn't a workaround — it's the format's design being used as advertised.

Four things make it correct, each of which has a test:

- **SYNC must precede SCHEMA in the prefix.** The reader's SYNC handler *clears*
  its schema map, because a sync frame rebinds every id (§6.6). Schema-first
  yields zero batches, silently.
- **Prefixes are cached per (segment, stream_id).** `stream_id` is meaningful
  only inside its segment; caching on the id alone would serve one segment's
  schema for another's frames, which decodes without error and gives wrong
  values.
- **`Truncated` after a range read is our bug, not the file's.** It means we
  handed the reader something not ending on a frame boundary. It is a loud
  error, never a silently short chart.
- **`Meta` and `Attachments` from a range reader are ignored entirely.**
  `time.anchor` is emitted after the records it dates (§5.2), so a range would
  legitimately miss it. Metadata comes from the whole-file scan and nowhere else.

One caveat worth repeating: the synthesized byte stream is **not a valid file**.
It carries only the requested DATA frames, which can break the per-run
contiguity §6.5 requires. The reader does not care — contiguity is enforced only
by the writer — but those bytes must never be handed to a user as an exported
range without regrouping by run first.

## The one core change

Phase 2 added `Reader.OnSchema func(*Schema, uint16)` to the core library. Ten
lines, no format change, no new dependency.

`Next` only ever hands out a schema attached to a batch, so a stream declared
with zero DATA frames was invisible — and a channel configured but never
triggered is an ordinary thing for a file to contain. `cmd/logbdump` has the
same blind spot today and could use the hook to fix it.

## Three things that are not incidental

These are properties of the format, and they are easy to break while making the
UI prettier.

**An absent sample is not a zero.** A guarded field whose guard does not hold
returns `logb.ErrFieldAbsent`, and SPEC §6.2 is explicit that this means the
field is not in the record. It travels as `present=false` internally, as `null`
on the wire, and as a break in the line on screen. Plotting a zero there
reproduces exactly the bug the guard feature exists to prevent, and it looks
entirely plausible.

**An enumeration is not a number.** The mean of "reverse" and "third" is not a
gear. Categorical fields are refused by `/api/series` rather than being given a
min/max envelope, and where a bucket is too narrow to resolve to one state it
is marked `mixed` and drawn hatched rather than having one of its values picked.

**Time is int64 ticks.** Absolute unix nanoseconds are about 1.7e18, five orders
of magnitude past 2^53. Serialising them as JSON numbers would quantise them to
roughly 256 ns while the chart still looked fine. Axis values are rebased to a
per-file epoch at index time and stay exact.

## How scale works

Two tiers, and the choice between them is the whole story.

**Tier 1, statistics.** The index pass has to decompress every frame anyway just
to find the frames, so while each batch is hot it also records, per field, the
min, max, first, last and present-count. That is about 40 bytes per field per
frame — 20 MB for 10k frames of 50 fields, against the tens of gigabytes the
samples themselves would occupy. A whole-file overview is answered from these
without decoding anything.

**Tier 2, exact.** When a window spans few enough frames to decode inside a
budget (256 by default), those frames are decoded through the Phase 2 accessor
and reduced from the samples. That is what the user sees zoomed in, which is
when exactness matters. A bounded LRU of decoded frames makes panning cheap.

A DATA frame is the unit of both, because it is the smallest thing that can be
decompressed. That also means no intermediate pyramid level is needed: frames
are already the granularity.

The safety property, and it has a test: **a Tier 1 envelope must always contain
the Tier 2 one.** Statistics are coarser, so a frame's bounds are applied to
every bucket it covers. That widens the envelope rather than narrowing it, which
is the safe direction — a spike stays visible, and no bucket ever claims a
tighter range than the data supports. A one-in-five-million spike survives a
whole-file overview of 100 million records.

Categorical fields get the same treatment in reverse: at frame granularity a
band that held more than one value is marked `mixed` and drawn hatched, because
Tier 1 genuinely does not know which value or in what order. Zoom in and it
resolves.

## The record table

A chart is a summary. The table is what it is a summary of, over the same
window, which is what makes the two checkable against each other — and against
`cmd/logbdump`, whose value formatting it deliberately mirrors.

Paging does not decode what it can count. A frame lying wholly inside the window
holds exactly the record count it declares, so skipping past it costs no
decompression at all; only the partially-overlapping frames at the window's
edges have to be decompressed to know which of their records are in range. That
is what makes an offset deep into a hundred-million-record file affordable, and
it has a test asserting that reaching the last frame touches exactly one.

Two columns per field, and the reason is the absence rule again. `text` is what
a human reads — an enumeration's name, a hex blob. `num` is the same value as a
number where one exists, which is what a CSV column needs, and for a categorical
field it is the *raw* value rather than the label. An absent field is empty in
both, shows as a dash on screen, and is an empty CSV cell. Never a zero.

**Export refuses rather than truncates.** A CSV has no way to say "and there was
more" — it just ends. So an over-large window is refused up front with a 400
saying how large it was, checked against the frame index before a byte is
decoded, rather than by a response that stops in the middle and looks complete.
The time column is written as integer ticks: the default float formatting turns
`4999970000` into `4.99997e+09`, which is the same number but reads as an
approximation and is the shape a spreadsheet rounds on import.

## Event lanes

A string or byte field has no y value to plot — the mean of two log messages is
not a log message — so it was previously not plottable at all. But it does have
a position on the axis and something to say there, and a recording's most
informative stream is often exactly that. Those fields are their own class now
(`event`), and they draw as a lane of marks. `ClassBlob` is left meaning what it
says: nothing to draw, which is true only of complex values.

The same two tiers as everywhere else. Under a couple of thousand events in the
window, the lane shows every one with its label. Past that it becomes a density
profile built from Tier 1 presence counts, which costs no decoding at all —
that is why the index pass now counts presence for event fields even though
there is no number to summarise. The two make different claims and the pane says
which it is showing: a bar means "this many happened somewhere in this span",
not "one happened at this x".

An absent event is not an event. A guarded string field whose guard does not
hold was not in the record (SPEC §6.2), and a mark there would claim something
happened when nothing did.

## Every pane shares one axis, literally

This is stated elsewhere in this file and it was not true. State bands and event
lanes drew on a plain canvas from edge to edge while uPlot inset its plot area
to make room for the y-axis, so an event at t=1.0 sat about ninety pixels from
t=1.0 on the chart above it — close enough to look deliberate.

Lanes are uPlot instances now. They have no series to draw; they hand their
canvas to a draw function from a `draw` hook. That fixes alignment by
construction rather than by matching constants: both panes feed the same axis
configuration to the same layout code, so the plot areas agree whatever uPlot
decides that means internally. Drag-to-zoom and the synced cursor come along
for free, and a few hundred lines of hand-rolled projection and drag handling
went away.

Two things in `layout.ts` are there because getting them wrong renders nothing
at all, with no exception and no console warning:

- **A lane must state its x range** rather than let uPlot derive one. uPlot
  takes the range from its visible series, and a lane has no visible series, so
  `valToPos` returns `NaN` and every mark lands off the canvas.
- **`u.bbox` is in canvas device pixels; `valToPos` returns CSS pixels** unless
  asked otherwise, and it already includes the plot-area offset when it is.
  Adding the two together puts everything off the edge.

`drive.py` now asserts both properties on every run — that every pane's plot
area has the same left and right edge, and that no lane canvas is entirely
transparent. Neither shows up as an error otherwise; the pane header will still
confidently report "7 events" over an empty lane.

## Runs and non-time axes

A stepped sweep is N traces sharing an axis, not one trace (SPEC §6.5). The tree
offers both readings: a run on its own, or every run overlaid on one pane, which
is how a sweep is actually read — the whole point of the repeat is comparing the
runs. They are never merged into a single series. Averaging four temperature
corners produces a curve the part has at no temperature, and it would look
entirely reasonable.

Every run is fetched over the same window with the same bucket count, so their
bucket positions coincide and uPlot can hold them in one aligned frame. Runs
need not cover the same extent — that is often the point — so a short run is
padded with nulls and ends, rather than being stretched to fit.

The axis is a tagged union (§5): time is an int64 count of ticks, everything
else an IEEE f64. A frequency sweep is usually logarithmic, and a decade sweep
drawn on a linear axis puts nine tenths of its width in the last decade — which
is not a cosmetic problem, because the corner frequency is what the sweep was
run to find and it lands in the crushed part. The x scale follows the stream's
axis mode, and `decimate` already buckets in log space to match. SPEC §5.3
rejects a logarithmic time axis, so a time stream is always linear.

Neither of these had a fixture. `logbgen -sweep` writes one: an AC response
swept over five decades and repeated at four temperatures, which exercises the
non-time axis, the log spacing and the runs at once.

## The frame map

A visual `cmd/logbdump`. The file is drawn to scale with each DATA frame in its
stream's colour over its segment's extent, so the visible gaps are the framing
itself — header, schemas, runs, metadata, attachments.

It is here because the layout is what makes the rest of the viewer possible.
Segment boundaries are where a damaged file can be picked up again (rule 3), a
frame is the unit of every decode and of every Tier 1 statistic, and the records
per frame are what a decimation budget is spent on. When a chart looks wrong,
this is the next place to look.

The listing is capped, but the per-segment totals are computed over every frame,
so the summary stays complete when the list is not — and the response says it
was capped rather than showing the first few thousand as if they were all.

## The sidecar cache

`<file>.logbview` beside the file, falling back to the user cache directory when
that directory is read-only — a log pulled off an SD card is exactly the thing
someone opens a viewer on. It holds Tier 0 and Tier 1 but **not the schemas**:
those are recovered by replaying the SCHEMA frames the index points at, so they
always come from the file rather than from a copy that could have drifted.

The version is bumped whenever the cached shape changes, and a mismatch
discards the cache rather than migrating it. Tier 1 gaining presence counts for
event fields is exactly that case: an older cache would give every event lane a
density of zero — a wrong chart, not a slow one.

It is only ever an accelerator. Every failure to read, validate or write it
falls back to scanning — SPEC §9's rule about the format's own index applies
just as well to ours.

A Logb file legitimately grows: a logger appends while someone is reading. When
the cache is valid but the file is longer, only the new tail is scanned, resuming
from the last cached segment. That works for the same reason random access does:
nothing points forward, and every segment restates its schemas.

## Layout

```
index/       scan, Tier 0 frame index, Tier 1 statistics, random access, sidecar
decimate/    min/max envelopes, categorical run-lengths, log-space bucketing
query/       tier selection and the decoded-frame cache
server/      stdlib net/http API
web/         TypeScript + Preact + uPlot frontend
web/dist/    build output, committed and embedded — see below
cmd/logbview CLI
```

## API

Axis values are epoch-relative: ticks of 10^`axisExp` seconds for a time axis,
the axis unit otherwise.

| Endpoint | Purpose |
|---|---|
| `GET /api/file` | streams, fields, runs, metadata, attachments, status |
| `GET /api/series?stream=&field=&run=&from=&to=&points=` | min/max envelope; `null` bounds are gaps |
| `GET /api/states?stream=&field=&run=&from=&to=&points=` | run-length states for categorical fields |
| `GET /api/series?...&format=bin` | the same, as typed arrays: no JSON parse, NaN carries absence |
| `GET /api/events?stream=&field=&run=&from=&to=` | event marks with labels, or per-frame density |
| `GET /api/frames?limit=` | the Tier 0 index: segments and DATA frames |
| `GET /api/records?stream=&run=&from=&to=&offset=&limit=` | decoded records; empty cells are absent fields |
| `GET /api/export.csv?stream=&run=&from=&to=&fields=` | the window as CSV |
| `GET /api/attach/{name}` | raw attachment bytes |
| `GET /api/progress` | SSE: indexing progress, then a `ready` event |

While indexing, every data endpoint answers `503` with a JSON progress body.
That is the honest code: the resource exists and is not available yet.

`stream` is the stream UUID, never `stream_id` — `stream_id` is segment-scoped
and rebound by every SYNC frame, which is what makes concatenation work
(SPEC §6.6).

## Generating fixtures

Only the small CAN example is checked in. The scale, sweep and run work is
unverifiable without the others, and they are too large or too incidental to
commit:

```
go run ./cmd/logbgen -big 100000000 -o big.logb    # 100M records, ~48 MB
go run ./cmd/logbgen -sweep 4 -o sweep.logb        # 4 runs on a log frequency axis
```

The big one carries a deliberately awkward feature: `boost` is guarded on
`mode`, so it is genuinely absent from most records. A viewer that treats absence as zero
draws a plausible-looking signal pinned to the bottom of the chart, and this is
the file that shows it.

## Checking the charts actually draw

Go tests prove the server sends correct numbers. They cannot prove the browser
draws them, and that gap is not theoretical: the binary series encoding shipped
with a 12-byte header, which made `new Float64Array(buf, 12, n)` throw and left
**every numeric chart blank** — while the Go round-trip test compared every byte
and passed. A `Float64Array` view must start on an 8-byte boundary; Go can read
any offset, so nothing on the server side could notice.

`web/tools/drive.py` closes that gap. It starts a viewer, drives a real headless
Chrome over the DevTools Protocol, clicks signals, drags to zoom, screenshots
both states, and reports any error the page rendered:

```
cd web/tools
python3 drive.py --file ../../../testdata/can-example.logb \
    --fields EngineSpeed,CoolantTemp,Gear,message --records
```

On every run it also asserts three things a screenshot cannot tell you: that
every pane's plot area has the same left and right edge, that no lane canvas is
entirely transparent, and that no frame in the map is drawn outside the strip
that represents the file.

`--records` also opens the drawer and checks both tabs — that the record table
drew rows with a consistent column count, and that the frame map drew its
segments and frames.

Screenshots land in `shots/`. It exits non-zero if a named signal is missing or
the page shows an error, so it works as a smoke test. `--url` drives a viewer
you already have running instead of starting one.

Needs `google-chrome` and the `websocket-client` Python package. No browser
extension, no npm install. Two things it handles that are easy to get wrong:
Chrome rejects the debugging websocket with a 403 unless launched with
`--remote-allow-origins`, and it re-parents itself away from the launching
process group, so shutdown goes through CDP `Browser.close` rather than a
process kill.

## Building the frontend

`web/dist` is committed on purpose. The Go toolchain will not run a bundler, so
`go install`ing this command has to work without a Node toolchain present.

```
cd web
npm install
npm run build     # tsc --noEmit && vite build
```

Then commit the regenerated `web/dist`.

## Development

`go.work` at the repository root (gitignored) makes this module build against
the working-tree reader. Without it, `viewer/go.mod`'s `replace` directive
points at `../`, which does the same thing less flexibly. Both come out once
the core has a tagged release.

## Licensing

MIT, matching the core. Every bundled component is permissive — uPlot MIT,
Preact MIT, klauspost/compress Apache-2.0, google/uuid BSD-3. Nothing is under
the GPL or any other copyleft license. See `LICENSE-THIRD-PARTY` and
`web/dist/LICENSES.txt`.

## Phases

1. **Done.** Walking skeleton: full-scan index, signal tree, charts.
2. **Done.** Random access: the `OnSchema` hook upstream, a Tier 0 frame index,
   and per-(segment, stream) synthesized prefixes feeding `io.SectionReader`
   ranges into the unmodified core reader.
3. **Done.** Scale: per-frame statistics computed during the index pass, exact
   on-the-fly decode over bounded frame sets, a binary wire format, and a
   sidecar cache that extends rather than rebuilds when a file grows.
4. **Done.** Completeness: record table, CSV export, event lanes, run overlays,
   non-time and logarithmic axes, and the frame-map inspector. Cursor readout
   across panes falls out of uPlot's synced cursors.
