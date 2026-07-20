import { useCallback, useEffect, useState } from "preact/hooks";
import type uPlot from "uplot";

import { axisToDisplay, displayToAxis, fetchEvents } from "./api";
import { Lane } from "./Lane";
import { laneGeom } from "./layout";
import type { Density, EventMark, Signal } from "./types";

interface Props {
  signal: Signal;
  from: number;
  to: number;
  onRange: (from: number, to: number) => void;
  onRemove: () => void;
}

const HEIGHT = 74;

/**
 * A string or byte field as a lane of marks.
 *
 * These fields have no y value to plot — the mean of two log messages is not a
 * log message — but they do have a position on the axis and something to say
 * there, and a recording's most informative stream is often exactly that. So
 * the lane draws where things happened and what they said, and nothing else.
 *
 * Zoomed out, the server sends per-frame counts rather than the events
 * themselves and the lane draws a density profile. That is a different claim
 * from a mark per event and it is labelled as one: a bar means "this many
 * things happened somewhere in this span", not "one thing happened at this x".
 */
export function EventLane({ signal, from, to, onRange, onRemove }: Props) {
  const [events, setEvents] = useState<EventMark[]>([]);
  const [density, setDensity] = useState<Density[]>([]);
  const [tier, setTier] = useState("");
  const [err, setErr] = useState<string | null>(null);
  const [hover, setHover] = useState<string | null>(null);

  const { stream, field } = signal;
  // Bands and lanes are single-run: two of them stacked in one strip would
  // occlude each other, so the tree never offers an overlay for these.
  const run = signal.runs[0];

  useEffect(() => {
    const ac = new AbortController();
    const a = displayToAxis(from, stream.axisKind, stream.axisExp);
    const b = displayToAxis(to, stream.axisKind, stream.axisExp);
    const show = (v: number) => axisToDisplay(v, stream.axisKind, stream.axisExp);

    fetchEvents(stream.uuid, field.name, run, a, b, ac.signal)
      .then((d) => {
        setErr(null);
        setTier(d.tier);
        setEvents((d.events ?? []).map((e) => ({ ...e, x: show(e.x) })));
        setDensity((d.density ?? []).map((k) => ({ ...k, x0: show(k.x0), x1: show(k.x1) })));
      })
      .catch((e) => {
        if (e.name !== "AbortError") setErr(String(e.message ?? e));
      });

    return () => ac.abort();
  }, [stream.uuid, field.name, run, from, to]);

  const draw = useCallback(
    (u: uPlot, g: CanvasRenderingContext2D) => {
      const { x, top, bottom, height, dpr } = laneGeom(u);
      const base = bottom - 2 * dpr;

      if (density.length > 0) {
        const peak = Math.max(...density.map((d) => d.n));
        g.fillStyle = "rgba(47,127,209,0.55)";
        for (const d of density) {
          const x0 = x(d.x0);
          const x1 = x(d.x1);
          const h = Math.max(2 * dpr, ((height - 6 * dpr) * d.n) / peak);
          g.fillRect(x0, base - h, Math.max(1, x1 - x0), h);
        }
        return;
      }

      g.strokeStyle = "#2f7fd1";
      g.lineWidth = dpr;
      for (const e of events) {
        const px = Math.round(x(e.x)) + 0.5;
        g.beginPath();
        g.moveTo(px, top + 4 * dpr);
        g.lineTo(px, base);
        g.stroke();
      }
    },
    [events, density],
  );

  /** What sits under the cursor, in the units the pane is drawn in. */
  const onCursor = useCallback(
    (v: number | null) => {
      if (v == null) {
        setHover(null);
        return;
      }
      if (density.length > 0) {
        const d = density.find((k) => v >= k.x0 && v <= k.x1);
        setHover(d ? `${d.n} event${d.n === 1 ? "" : "s"} in this frame` : null);
        return;
      }
      // Nearest mark within a few pixels' worth of axis units. Anything looser
      // would label a stretch of empty lane with a distant event.
      const tol = (to - from) / 200;
      let best: EventMark | null = null;
      let gap = Infinity;
      for (const e of events) {
        const d = Math.abs(e.x - v);
        if (d < gap) [best, gap] = [e, d];
      }
      setHover(best && gap <= tol ? best.label : null);
    },
    [events, density, from, to],
  );

  return (
    <div class="pane">
      <div class="pane-head">
        <span class="pane-title">
          {stream.name}.{field.name}
          {stream.runs.length > 1 && (
            <span class="muted"> · {stream.runs.find((r) => r.id === run)?.label ?? `run ${run}`}</span>
          )}
        </span>
        <span class="pane-note">
          {/* The two tiers make different claims, so the lane says which it is
              showing rather than letting a density bar pass for an event. */}
          {tier === "stats" ? (
            <span title="Too many events to draw individually. Bar height is how many fell in each frame; zoom in for the events themselves.">
              density
            </span>
          ) : (
            `${events.length} event${events.length === 1 ? "" : "s"}`
          )}
          {hover && <span class="muted"> · {hover}</span>}
        </span>
        <button class="close" onClick={onRemove} title="Remove">
          ×
        </button>
      </div>
      {err && <div class="err">{err}</div>}
      <Lane
        stream={stream}
        from={from}
        to={to}
        height={HEIGHT}
        onRange={onRange}
        draw={draw}
        onCursor={onCursor}
        revision={`${tier}:${events.length}:${density.length}`}
      />
    </div>
  );
}
