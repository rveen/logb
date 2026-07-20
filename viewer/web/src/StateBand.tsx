import { useCallback, useEffect, useState } from "preact/hooks";
import type uPlot from "uplot";

import { axisToDisplay, displayToAxis, fetchStates } from "./api";
import { Lane } from "./Lane";
import { laneGeom } from "./layout";
import type { Signal, State } from "./types";

interface Props {
  signal: Signal;
  from: number;
  to: number;
  onRange: (from: number, to: number) => void;
  onRemove: () => void;
}

const HEIGHT = 74;

/**
 * One categorical signal as a band of coloured runs.
 *
 * Deliberately not a line chart. The mean of "reverse" and "third" is not a
 * gear, and a line joining them claims the vehicle passed through the states
 * in between. A band states only what the file says: this value, from here to
 * here.
 *
 * Three cases get distinct treatment, because conflating them would invent
 * facts: a resolved state is filled and labelled; a bucket holding more than
 * one distinct value is hatched and left unlabelled; and a stretch where the
 * field was absent is left empty.
 */
export function StateBand({ signal, from, to, onRange, onRemove }: Props) {
  const [states, setStates] = useState<State[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [hover, setHover] = useState<State | null>(null);

  const { stream, field } = signal;
  // Bands and lanes are single-run: two of them stacked in one strip would
  // occlude each other, so the tree never offers an overlay for these.
  const run = signal.runs[0];

  useEffect(() => {
    const ac = new AbortController();
    const a = displayToAxis(from, stream.axisKind, stream.axisExp);
    const b = displayToAxis(to, stream.axisKind, stream.axisExp);
    const show = (v: number) => axisToDisplay(v, stream.axisKind, stream.axisExp);

    fetchStates(stream.uuid, field.name, run, a, b, 1000, ac.signal)
      .then((d) => {
        setErr(null);
        setStates(d.states.map((s) => ({ ...s, x0: show(s.x0), x1: show(s.x1) })));
      })
      .catch((e) => {
        if (e.name !== "AbortError") setErr(String(e.message ?? e));
      });

    return () => ac.abort();
  }, [stream.uuid, field.name, run, from, to]);

  const draw = useCallback(
    (u: uPlot, g: CanvasRenderingContext2D) => {
      const { x, top, height, dpr } = laneGeom(u);
      const y = top + 6 * dpr;
      const h = height - 12 * dpr;

      for (const s of states) {
        // An absent run is a hole in the record, drawn as nothing at all.
        if (s.absent) continue;

        const x0 = x(s.x0);
        const x1 = x(s.x1);
        const w = Math.max(1, x1 - x0);

        g.fillStyle = s.mixed ? hatch(g) : colorFor(s.raw);
        g.fillRect(x0, y, w, h);

        if (!s.mixed && w > 26 * dpr && s.label) {
          g.fillStyle = "#fff";
          g.font = `${11 * dpr}px ui-sans-serif, system-ui, sans-serif`;
          g.textBaseline = "middle";
          const text = fit(g, s.label, w - 8 * dpr);
          if (text) g.fillText(text, x0 + 4 * dpr, y + h / 2);
        }
      }
    },
    [states],
  );

  const onCursor = useCallback(
    (v: number | null) => {
      if (v == null) {
        setHover(null);
        return;
      }
      setHover(states.find((s) => v >= s.x0 && v <= s.x1 && !s.absent) ?? null);
    },
    [states],
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
          state{hover && <span class="muted"> · {hover.mixed ? "mixed" : hover.label}</span>}
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
        revision={states.length}
      />
    </div>
  );
}

/**
 * A stable colour per raw value. Deterministic so a state keeps its colour as
 * the user pans, which is what makes a band readable at a glance.
 */
function colorFor(raw: number): string {
  const hue = (Math.abs(Math.round(raw)) * 47) % 360;
  return `hsl(${hue} 55% 45%)`;
}

let hatchPattern: CanvasPattern | null = null;

/** Diagonal hatching marks a bucket too narrow to resolve to one state. */
function hatch(g: CanvasRenderingContext2D): string | CanvasPattern {
  if (hatchPattern) return hatchPattern;
  const tile = document.createElement("canvas");
  tile.width = tile.height = 8;
  const t = tile.getContext("2d");
  if (!t) return "#888";
  t.fillStyle = "#9aa4ae";
  t.fillRect(0, 0, 8, 8);
  t.strokeStyle = "#6b7681";
  t.lineWidth = 2;
  t.beginPath();
  t.moveTo(0, 8);
  t.lineTo(8, 0);
  t.stroke();
  hatchPattern = g.createPattern(tile, "repeat");
  return hatchPattern ?? "#888";
}

function fit(g: CanvasRenderingContext2D, text: string, max: number): string {
  if (g.measureText(text).width <= max) return text;
  for (let n = text.length - 1; n > 0; n--) {
    const t = text.slice(0, n) + "…";
    if (g.measureText(t).width <= max) return t;
  }
  return "";
}
