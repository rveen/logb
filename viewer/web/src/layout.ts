import type uPlot from "uplot";

import type { Stream } from "./types";

/**
 * The axis geometry every pane shares.
 *
 * Panes stack on a common x-axis, and that claim is only true if they agree
 * about where the plot area starts and ends. uPlot derives that inset from the
 * y-axis configuration, so the numbers below are pinned rather than left to
 * automatic fitting: a chart whose tick labels happen to be wider than its
 * neighbour's would otherwise shift its whole trace sideways relative to the
 * lane beneath it. Close enough to look deliberate, wrong enough to mislead.
 *
 * Nothing here is a pixel offset anyone measures against. Both chart and lane
 * hand these same values to the same layout code, which is what makes the plot
 * areas line up — whatever uPlot decides that means internally.
 */

/** Width of the y-axis tick column. */
export const AXIS_W = 56;

/** Width of the rotated y-axis label beside it. */
export const LABEL_W = 26;

/** Height of the x-axis below the plot area. */
export const AXIS_H = 34;

/**
 * Padding right of the plot area.
 *
 * Wide enough for half of the last x tick label, which is centred on the tick
 * at the very edge. A log frequency axis ends on something like "1,000,000",
 * and ten pixels clipped it to "1,000,0" — a number that reads as a plausible
 * different one rather than as damage.
 */
export const PAD_RIGHT = 34;

/** uPlot padding, in its [top, right, bottom, left] order. */
export const PADDING: uPlot.Padding = [8, PAD_RIGHT, 0, 0];

/**
 * The cursor shared by every pane. One key means moving the pointer over any
 * pane moves the readout on all of them, which is the whole point of stacking
 * them on one axis.
 */
export function sharedCursor(): uPlot.Cursor {
  return { sync: { key: "logb" }, drag: { x: true, y: false } };
}

/**
 * The x scale a stream wants.
 *
 * SPEC §5.3 rejects a logarithmic time axis, so a time stream is always linear
 * — but a frequency sweep usually is not. A decade sweep drawn on a linear axis
 * puts nine tenths of its width in the last decade and renders the first decade
 * as a single column, which is not a rendering nicety: the corner frequency,
 * the thing the sweep was run to find, lands in that column.
 *
 * `time: false` throughout. Axis values are epoch-relative ticks, not
 * milliseconds since 1970, and uPlot's date formatting would read them as the
 * latter.
 */
export function xScale(stream: Stream): uPlot.Scale {
  if (stream.axisMode === "log") {
    return { time: false, distr: 3 };
  }
  return { time: false };
}

/**
 * A stable colour per run in an overlay.
 *
 * Ordinal rather than hashed: runs in a sweep are a sequence, and a sequence
 * reads best as a progression. The hues are spread far enough that adjacent
 * runs stay distinguishable.
 */
export function runColor(i: number): string {
  return `hsl(${(i * 47 + 205) % 360} 65% 45%)`;
}

/**
 * The drawing frame for a lane's custom content.
 *
 * uPlot mixes two coordinate systems and the mix is easy to get wrong in a way
 * that silently draws nothing: `u.bbox` is in canvas device pixels, while
 * `valToPos` returns CSS pixels unless asked otherwise, and it already includes
 * the plot-area offset when it does. Adding the two together puts every mark
 * off the edge of the canvas. uPlot also leaves the context untransformed and
 * scales coordinates by the pixel ratio itself, so stroke widths and font sizes
 * have to be scaled here too or they come out half size on a retina display.
 *
 * All of that lives here so a lane can just ask for x, top and bottom.
 */
export function laneGeom(u: uPlot) {
  const dpr = window.devicePixelRatio || 1;
  return {
    /** Canvas x for an axis value. */
    x: (v: number) => u.valToPos(v, "x", true),
    left: u.bbox.left,
    right: u.bbox.left + u.bbox.width,
    top: u.bbox.top,
    bottom: u.bbox.top + u.bbox.height,
    height: u.bbox.height,
    dpr,
  };
}

/**
 * The y-axis a lane uses: the same width as a chart's, with no ticks and no
 * values. It reserves the identical gutter without pretending the lane has a
 * quantity on it, which it does not — the mean of two log messages is not a log
 * message.
 */
export function LANE_AXES(): uPlot.Axis[] {
  return [
    { show: false, size: 0 },
    {
      label: " ",
      labelSize: LABEL_W,
      size: AXIS_W,
      ticks: { show: false },
      grid: { show: false },
      values: () => [],
    },
  ];
}
