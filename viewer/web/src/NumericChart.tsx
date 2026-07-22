import { useEffect, useRef, useState } from "preact/hooks";
import uPlot from "uplot";
import "uplot/dist/uPlot.min.css";

import { axisLabel, axisToDisplay, displayToAxis, fetchSeriesBinary } from "./api";
import { formatTimeWithUnit, timeAxisLabel, timeTicks } from "./axis";
import { AXIS_H, AXIS_W, LABEL_W, PADDING, runColor, sharedCursor, xScale } from "./layout";
import type { Signal } from "./types";

interface Props {
  signal: Signal;
  /** Shared view window, in display units. */
  from: number;
  to: number;
  onRange: (from: number, to: number) => void;
  onRemove: () => void;
}

const HEIGHT = 180;

/**
 * A numeric signal as a min/max envelope, once per run.
 *
 * The server sends two bounds per bucket rather than one value, so a spike
 * narrower than a pixel still reaches the screen. Where a bucket held no
 * present sample both bounds are null and uPlot breaks the line — that gap is
 * a fact about the recording, not a rendering artefact (SPEC §6.2).
 *
 * Several runs share the pane and keep their own traces. They are never
 * combined: a stepped sweep is N measurements of the same quantity under
 * different conditions (§6.5), and averaging four temperature corners produces
 * a curve the part has at no temperature. Every run is fetched over the same
 * window with the same bucket count, so their x positions coincide and uPlot
 * can hold them in one aligned frame.
 */
export function NumericChart({ signal, from, to, onRange, onRemove }: Props) {
  const host = useRef<HTMLDivElement>(null);
  const plot = useRef<uPlot | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [exact, setExact] = useState(false);
  const [tier, setTier] = useState("");
  const [gaps, setGaps] = useState(0);

  // Suppresses the setScale hook while we are the ones setting the scale, so
  // pushing a range down does not bounce straight back up as a new range.
  const applying = useRef(false);

  const { stream, field, runs } = signal;
  const runKey = runs.join("+");
  const overlay = runs.length > 1;

  useEffect(() => {
    if (!host.current) return;

    // A time axis is drawn in whatever unit its window calls for, so both the
    // axis title and its tick labels are computed per draw rather than pinned
    // at build time. Every other axis kind already arrives in its own unit.
    const time = stream.axisKind === "time";
    const spanOf = (u: uPlot) => (u.scales.x.max ?? 0) - (u.scales.x.min ?? 0);

    const series: uPlot.Series[] = [
      {
        label: axisLabel(stream.axisKind, stream.axisUnit),
        value: time ? (u, v) => (v == null ? "" : formatTimeWithUnit(v, spanOf(u))) : undefined,
      },
    ];
    const bands: uPlot.Band[] = [];
    runs.forEach((r, i) => {
      const color = overlay ? runColor(i) : "#2f7fd1";
      const name = labelForRun(stream, r);
      // An overlay gets one legend entry per run rather than two. Eight rows of
      // "temperature=85 degC max" is not a legend, it is a wall — and the pair
      // is one trace conceptually: an envelope, drawn as its two bounds. The
      // upper bound's row is hidden by class rather than by dropping the
      // series, which still has to exist for the band to reference.
      series.push({ label: overlay ? name : "min", stroke: color, width: 1, spanGaps: false });
      series.push({
        label: overlay ? `${name} (upper)` : "max",
        class: overlay ? "lg-hide" : undefined,
        stroke: color,
        width: 1,
        spanGaps: false,
      });
      // The band between the two bounds is the envelope. When the range is
      // exact the bounds coincide and it collapses to a plain line.
      const min = 1 + i * 2;
      bands.push({ series: [min + 1, min], fill: fade(color) });
    });

    const opts: uPlot.Options = {
      width: host.current.clientWidth || 800,
      height: HEIGHT,
      scales: { x: xScale(stream) },
      cursor: sharedCursor(),
      legend: { show: true },
      series,
      bands,
      // Sizes pinned rather than left to uPlot's automatic fitting, so the
      // state bands and event lanes put a value at the same pixel this chart
      // does. See layout.ts.
      padding: PADDING,
      axes: [
        time
          ? {
              label: (u: uPlot) => timeAxisLabel(spanOf(u)),
              size: AXIS_H,
              values: (u: uPlot, splits: number[]) => timeTicks(splits, spanOf(u)),
            }
          : { label: axisLabel(stream.axisKind, stream.axisUnit), size: AXIS_H },
        { label: field.unit || field.name, labelSize: LABEL_W, size: AXIS_W },
      ],
      hooks: {
        setScale: [
          (u: uPlot, key: string) => {
            if (key !== "x" || applying.current) return;
            const { min, max } = u.scales.x;
            if (min != null && max != null) onRange(min, max);
          },
        ],
      },
    };

    const empty: uPlot.AlignedData = [[], ...runs.flatMap(() => [[], []])] as uPlot.AlignedData;
    const u = new uPlot(opts, empty, host.current);
    plot.current = u;

    const ro = new ResizeObserver(() => {
      if (host.current) u.setSize({ width: host.current.clientWidth, height: HEIGHT });
    });
    ro.observe(host.current);

    return () => {
      ro.disconnect();
      u.destroy();
      plot.current = null;
    };
    // Rebuilt only when the signal itself changes; range changes flow through
    // the data effect below.
  }, [stream.uuid, field.name, runKey]);

  useEffect(() => {
    const ac = new AbortController();
    const width = host.current?.clientWidth ?? 800;
    const points = Math.max(2, Math.round(width));

    const a = displayToAxis(from, stream.axisKind, stream.axisExp);
    const b = displayToAxis(to, stream.axisKind, stream.axisExp);

    Promise.all(
      runs.map((r) => fetchSeriesBinary(stream.uuid, field.name, r, a, b, points, ac.signal)),
    )
      .then((all) => {
        setErr(null);
        setExact(all.every((d) => d.exact));
        // The pane reports the weakest tier it is showing. Saying "exact"
        // because one run of four happened to be would overstate the rest.
        setTier(all.some((d) => d.tier === "stats") ? "stats" : "exact");
        setGaps(all.reduce((n, d) => n + d.n.filter((k) => k === 0).length, 0));

        // Every run was asked for the same window and bucket count, so the
        // bucket positions coincide; the longest is the frame's x.
        const widest = all.reduce((w, d) => (d.x.length > w.x.length ? d : w), all[0]);
        const xs = widest.x.map((v) => axisToDisplay(v, stream.axisKind, stream.axisExp));
        const cols: (number | null)[][] = [];
        for (const d of all) {
          cols.push(pad(d.min, xs.length));
          cols.push(pad(d.max, xs.length));
        }

        const u = plot.current;
        if (!u) return;
        applying.current = true;
        u.setData([xs, ...cols] as uPlot.AlignedData);
        u.setScale("x", { min: from, max: to });
        applying.current = false;
      })
      .catch((e) => {
        if (e.name !== "AbortError") setErr(String(e.message ?? e));
      });

    return () => ac.abort();
  }, [stream.uuid, field.name, runKey, from, to]);

  return (
    <div class="pane">
      <div class="pane-head">
        <span class="pane-title">
          {stream.name}.{field.name}
          {stream.runs.length > 1 && (
            <span class="muted">
              {" · "}
              {overlay ? `${runs.length} runs` : labelForRun(stream, runs[0])}
            </span>
          )}
        </span>
        <span class="pane-note">
          {/* "stats" means the answer came from per-frame summaries rather
              than the samples: correct bounds, frame-coarse resolution. */}
          {tier === "stats" ? (
            <span title="Reduced from per-frame statistics. Zoom in for exact samples.">overview</span>
          ) : exact ? (
            "exact"
          ) : (
            "min/max"
          )}
          {gaps > 0 && <span class="gap-note" title="buckets with no present sample"> · {gaps} gaps</span>}
        </span>
        <button class="close" onClick={onRemove} title="Remove">
          ×
        </button>
      </div>
      {err && <div class="err">{err}</div>}
      <div ref={host} class="plot" />
    </div>
  );
}

/**
 * What distinguishes a run, for a legend entry. The parameters that were
 * stepped are the only thing that makes a sweep legible; "run 2" is not.
 */
function labelForRun(stream: Signal["stream"], id: number): string {
  return stream.runs.find((r) => r.id === id)?.label ?? `run ${id}`;
}

/**
 * Pads a run's bounds to the frame's length with nulls.
 *
 * Runs need not cover the same extent — that is often the point of a sweep —
 * and a short run must end rather than be stretched. Null is a gap, which is
 * the same thing an absent sample is.
 */
function pad(v: (number | null)[], n: number): (number | null)[] {
  if (v.length >= n) return v;
  return [...v, ...new Array<null>(n - v.length).fill(null)];
}

function fade(color: string): string {
  return color.replace("hsl(", "hsla(").replace(")", " / 0.18)");
}
