import { useEffect, useRef } from "preact/hooks";
import uPlot from "uplot";

import { axisLabel } from "./api";
import { LANE_AXES, PADDING, sharedCursor, xScale } from "./layout";
import type { Stream } from "./types";

interface Props {
  stream: Stream;
  /** Shared view window, in display units. */
  from: number;
  to: number;
  height: number;
  onRange: (from: number, to: number) => void;
  /** Draws the lane's content into uPlot's canvas, over the plot area. */
  draw: (u: uPlot, g: CanvasRenderingContext2D) => void;
  /** Called with the axis value under the cursor, or null when it leaves. */
  onCursor?: (value: number | null) => void;
  /** Changing this redraws without rebuilding the plot. */
  revision: unknown;
}

/**
 * An empty uPlot that hands its canvas to a custom draw function.
 *
 * State bands and event lanes have no y value to plot, so the obvious thing is
 * a plain canvas. That was the first implementation and it was wrong in a way
 * that looked right: uPlot insets its plot area to make room for the y-axis and
 * sizes that inset from its own tick labels, so a hand-drawn lane put t=1.0
 * dozens of pixels away from t=1.0 on the chart above it. Panes that claim to
 * share an axis have to actually share it.
 *
 * Handing the drawing to uPlot fixes that by construction rather than by
 * matching constants: both panes feed the same axis configuration to the same
 * layout code, so the plot areas line up whatever uPlot decides that means.
 * Drag-to-zoom and the synced cursor come along for free.
 */
export function Lane({ stream, from, to, height, onRange, draw, onCursor, revision }: Props) {
  const host = useRef<HTMLDivElement>(null);
  const plot = useRef<uPlot | null>(null);
  const drawRef = useRef(draw);
  const applying = useRef(false);
  // The window, readable from uPlot's range callback without rebuilding the
  // plot every time it changes.
  const span = useRef<[number, number]>([from, to]);
  span.current = [from, to];

  // Kept in a ref so a new closure over fresh data does not rebuild the plot.
  drawRef.current = draw;

  useEffect(() => {
    if (!host.current) return;

    const opts: uPlot.Options = {
      width: host.current.clientWidth || 800,
      height,
      // The x range is stated rather than derived. uPlot normally takes it
      // from the data of its visible series, and a lane has no visible series
      // — which left valToPos returning NaN and the lane drawing nothing at
      // all, with no error anywhere to say so.
      scales: {
        x: { ...xScale(stream), auto: false, range: () => span.current },
        y: { range: [0, 1] },
      },
      cursor: sharedCursor(),
      legend: { show: false },
      // uPlot needs the series shape; nothing is drawn from it.
      series: [{ label: axisLabel(stream.axisKind, stream.axisUnit) }, { show: false }],
      padding: PADDING,
      axes: LANE_AXES(),
      hooks: {
        draw: [
          (u: uPlot) => {
            const g = u.ctx;
            g.save();
            // Clip to the plot area so a band running past the window cannot
            // paint over the axis gutter.
            g.beginPath();
            g.rect(u.bbox.left, u.bbox.top, u.bbox.width, u.bbox.height);
            g.clip();
            drawRef.current(u, g);
            g.restore();
          },
        ],
        setScale: [
          (u: uPlot, key: string) => {
            if (key !== "x" || applying.current) return;
            const { min, max } = u.scales.x;
            if (min != null && max != null) onRange(min, max);
          },
        ],
        setCursor: [
          (u: uPlot) => {
            if (!onCursor) return;
            const left = u.cursor.left;
            if (left == null || left < 0) {
              onCursor(null);
              return;
            }
            onCursor(u.posToVal(left, "x"));
          },
        ],
      },
    };

    const u = new uPlot(opts, [[from, to], [0, 0]] as uPlot.AlignedData, host.current);
    plot.current = u;

    const ro = new ResizeObserver(() => {
      if (host.current) u.setSize({ width: host.current.clientWidth, height });
    });
    ro.observe(host.current);

    return () => {
      ro.disconnect();
      u.destroy();
      plot.current = null;
    };
  }, [stream.uuid, height]);

  useEffect(() => {
    const u = plot.current;
    if (!u) return;
    applying.current = true;
    u.setData([[from, to], [0, 0]] as uPlot.AlignedData);
    u.setScale("x", { min: from, max: to });
    applying.current = false;
    u.redraw();
  }, [from, to, revision]);

  return <div ref={host} class="plot lane" />;
}
