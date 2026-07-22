/**
 * Time-axis units.
 *
 * The axis carries seconds, and a transient analysis is routinely microseconds
 * long. Formatted as seconds that is unreadable in the literal sense: uPlot's
 * default numeric formatter stops at three decimals, so a 1 ms sweep labels half
 * its ticks "0" and the other half "0.001" — an axis with two distinct values on
 * it, which is no axis at all.
 *
 * So the unit is chosen from the window rather than fixed at seconds, and the
 * tick labels are numbers in that unit: 0, 0.1, … 1 with the axis reading
 * "time (ms)". Values stay in seconds everywhere else — this is presentation,
 * and nothing downstream has to know about it.
 */

export interface TimeUnit {
  /** Seconds per unit: 1e-3 for ms. */
  factor: number;
  unit: string;
}

/** Coarsest first, which is the order the choice below wants. */
const TIME_UNITS: TimeUnit[] = [
  { factor: 1, unit: "s" },
  { factor: 1e-3, unit: "ms" },
  { factor: 1e-6, unit: "µs" },
  { factor: 1e-9, unit: "ns" },
  { factor: 1e-12, unit: "ps" },
  { factor: 1e-15, unit: "fs" },
];

const SECONDS: TimeUnit = TIME_UNITS[0];

/**
 * The unit a window of `span` seconds reads best in: the coarsest one the span
 * is at least one of.
 *
 * The span decides, not the values, because the span is what the labels have to
 * resolve. Zoomed into a microsecond of a run that started an hour in, that
 * makes for long labels — but long and distinct, where the alternative is short
 * and identical.
 */
export function timeUnit(span: number): TimeUnit {
  if (!isFinite(span) || span <= 0) return SECONDS;
  for (const u of TIME_UNITS) {
    if (span >= u.factor) return u;
  }
  return TIME_UNITS[TIME_UNITS.length - 1];
}

/**
 * Decimals needed to keep two values `step` apart from printing the same.
 *
 * One extra digit past the step, so ticks a step apart differ in a digit that
 * is actually shown, and capped: past six the label is noise.
 */
export function decimalsFor(step: number): number {
  if (!isFinite(step) || step <= 0) return 3;
  return Math.min(6, Math.max(0, Math.ceil(-Math.log10(step))));
}

/** Formats one value in seconds as a number of `u`, without the unit. */
export function formatTime(v: number, u: TimeUnit, decimals: number): string {
  return (v / u.factor).toFixed(decimals);
}

/** Formats a value in seconds with its unit, e.g. "50.0 µs". */
export function formatTimeWithUnit(v: number, span: number): string {
  const u = timeUnit(span);
  // A cursor readout wants more resolution than a tick label: the point of
  // hovering is to read the sample, not the gridline. A thousandth of the
  // window is finer than a pixel on any screen this runs on.
  return `${formatTime(v, u, decimalsFor(span / 1000 / u.factor))} ${u.unit}`;
}

/**
 * Tick labels for a time axis, in the unit its window calls for.
 *
 * uPlot has already chosen the tick positions; this only decides how they read.
 */
export function timeTicks(splits: number[], span: number): string[] {
  const u = timeUnit(span);
  const step = splits.length > 1 ? Math.abs(splits[1] - splits[0]) : span;
  const decimals = decimalsFor(step / u.factor);
  return splits.map((v) => formatTime(v, u, decimals));
}

/** The x-axis title for a time axis over a window, e.g. "time (ms)". */
export function timeAxisLabel(span: number): string {
  return `time (${timeUnit(span).unit})`;
}
