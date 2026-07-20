import { useEffect, useMemo, useState } from "preact/hooks";

import { fetchFrames } from "./api";
import type { FrameMapData, Stream } from "./types";

interface Props {
  streams: Stream[];
}

/**
 * The file's byte layout: where every segment and DATA frame sits.
 *
 * A visual cmd/logbdump, and it is here because the layout is what makes the
 * rest of the viewer possible. Segment boundaries are where a damaged file can
 * be picked up again (rule 3); a frame is the unit of every decode and of every
 * Tier 1 statistic; the records per frame are what a decimation budget is spent
 * on. When a chart looks wrong, this is the next place to look.
 */
export function FrameMap({ streams }: Props) {
  const [data, setData] = useState<FrameMapData | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [pick, setPick] = useState<string | null>(null);

  useEffect(() => {
    const ac = new AbortController();
    fetchFrames(ac.signal)
      .then((d) => {
        setData(d);
        setErr(null);
      })
      .catch((e) => {
        if (e.name !== "AbortError") setErr(String(e.message ?? e));
      });
    return () => ac.abort();
  }, []);

  // A colour per stream, assigned by position so it is stable across renders
  // and matches between the strip and the table.
  const color = useMemo(() => {
    const m = new Map<string, string>();
    streams.forEach((s, i) => m.set(s.uuid, `hsl(${(i * 67 + 200) % 360} 60% 50%)`));
    return m;
  }, [streams]);

  if (err) return <div class="err">{err}</div>;
  if (!data) return <p class="muted pad">Reading the frame index…</p>;

  const pct = (n: number) => `${(n / Math.max(1, data.size)) * 100}%`;

  // Frames from every stream share the strip, but the axis columns are only
  // labelled when the file speaks one language. A time stream counts ticks and
  // a frequency stream counts hertz; naming one of them for both would be wrong
  // for the other.
  const kinds = new Set(streams.map((s) => s.axisKind));
  const unit =
    kinds.size === 1 && streams[0]
      ? streams[0].axisKind === "time"
        ? ` (1e${streams[0].axisExp} s)`
        : streams[0].axisUnit
          ? ` (${streams[0].axisUnit})`
          : ""
      : "";

  return (
    <div class="table-scroll framemap">
      <div class="fm-summary muted">
        {data.size.toLocaleString()} bytes · {data.segments.length} segment
        {data.segments.length === 1 ? "" : "s"} · {data.total.toLocaleString()} DATA frames
        {data.truncated && (
          <span title="Only the first frames are listed; the per-segment totals below cover all of them.">
            {" "}
            · listing capped at {data.frames.length.toLocaleString()}
          </span>
        )}
      </div>

      {/* The file as a bar, to scale. Frames are drawn over segment extents, so
          the gaps are the framing itself: headers, schemas, runs, metadata. */}
      <div class="fm-strip">
        {data.segments.map((s) => (
          <div
            key={s.index}
            class="fm-seg"
            style={{ left: pct(s.offset), width: pct(Math.max(1, s.end - s.offset)) }}
            title={`segment ${s.index}: ${s.schemas} schemas, ${s.runs} runs, ${s.frames} frames, ${s.records.toLocaleString()} records`}
          />
        ))}
        {data.frames.map((f) => (
          <div
            key={f.offset}
            class={`fm-frame${pick && pick !== f.uuid ? " dim" : ""}`}
            style={{
              left: pct(f.offset),
              width: pct(Math.max(1, f.size)),
              background: color.get(f.uuid) ?? "#888",
            }}
            title={`${f.stream} @${f.offset} · ${f.size} B · ${f.records} records · run ${f.run} · segment ${f.segment}`}
          />
        ))}
      </div>

      <div class="fm-legend">
        {streams.map((s) => (
          <button
            key={s.uuid}
            class={`fm-key${pick === s.uuid ? " on" : ""}`}
            onClick={() => setPick(pick === s.uuid ? null : s.uuid)}
            title="Highlight this stream's frames"
          >
            <span class="swatch" style={{ background: color.get(s.uuid) }} />
            {s.name}
          </button>
        ))}
      </div>

      <table class="records">
        <thead>
          <tr>
            <th class="num">offset</th>
            <th class="num">bytes</th>
            <th class="num">seg</th>
            <th>stream</th>
            <th class="num">run</th>
            <th class="num">records</th>
            {/* Raw axis units, not the seconds the record table shows: this is
                the index's own view, and the numbers here are exactly what
                selectFrames compares against. */}
            <th class="num">axis from{unit}</th>
            <th class="num">axis to{unit}</th>
          </tr>
        </thead>
        <tbody>
          {data.frames
            .filter((f) => !pick || f.uuid === pick)
            .slice(0, 500)
            .map((f) => (
              <tr key={f.offset}>
                <td class="num">{f.offset.toLocaleString()}</td>
                <td class="num">{f.size.toLocaleString()}</td>
                <td class="num">{f.segment}</td>
                <td>
                  <span class="swatch" style={{ background: color.get(f.uuid) }} />
                  {f.stream}
                </td>
                <td class="num">{f.run}</td>
                <td class="num">{f.records.toLocaleString()}</td>
                <td class="num">{f.first.toLocaleString()}</td>
                <td class="num">{f.last.toLocaleString()}</td>
              </tr>
            ))}
        </tbody>
      </table>
    </div>
  );
}
