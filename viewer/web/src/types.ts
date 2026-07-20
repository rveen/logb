// Mirrors the DTOs in ../../server/api.go. Kept hand-written and small rather
// than generated: the API is a handful of shapes and the duplication is easier
// to read than a codegen step.

export type Class = "numeric" | "categorical" | "event" | "blob";

export interface Field {
  index: number;
  name: string;
  unit: string;
  desc: string;
  type: string;
  class: Class;
  /** Values may legitimately be absent; the tree shows this as "sparse". */
  guarded: boolean;
  /** This field carries the stream's independent variable. */
  isAxis: boolean;
  bitOffset: number;
  bitWidth: number;
  bigEndian: boolean;
  variable: boolean;
  conv: string;
  meta: Record<string, string> | null;
  plottable: boolean;
}

export interface Run {
  id: number;
  index: number;
  label: string;
  params: Record<string, string> | null;
}

export interface Stream {
  uuid: string;
  name: string;
  axisKind: string;
  axisMode: string;
  /** One axis tick is 10^axisExp seconds. Time axes only. */
  axisExp: number;
  axisUnit: string;
  axisMin: number;
  axisMax: number;
  hasSpan: boolean;
  records: number;
  meta: Record<string, string> | null;
  runs: Run[];
  fields: Field[];
}

export interface FileInfo {
  path: string;
  size: number;
  /** Absolute ticks of the earliest sample; every axis value is relative to it. */
  epoch: number;
  hasEpoch: boolean;
  /** The scan stopped at damage. Still a valid file under rule 2. */
  truncated: boolean;
  /** An END frame was seen — not necessarily at end of file (SPEC §6.6). */
  closed: boolean;
  unsupported: string[];
  meta: { key: string; value: string }[];
  attachments: { name: string; size: number }[];
  streams: Stream[];
}

export interface SeriesData {
  stream: string;
  field: string;
  unit: string;
  run: number;
  /** No decimation happened; x/min/max are the samples themselves. */
  exact: boolean;
  /**
   * Which path answered: "exact" decoded the samples, "stats" reduced
   * per-frame summaries without decoding. Shown rather than hidden, so the
   * chart never implies a precision it does not have.
   */
  tier?: string;
  x: number[];
  /** null where the bucket held no present sample. Must render as a gap. */
  min: (number | null)[];
  max: (number | null)[];
  n: number[];
}

export interface State {
  x0: number;
  x1: number;
  raw: number;
  label: string;
  /** More than one distinct value fell in this bucket; do not name one. */
  mixed: boolean;
  absent: boolean;
}

export interface StatesData {
  stream: string;
  field: string;
  run: number;
  tier?: string;
  states: State[];
}

/** One occurrence in an event lane. */
export interface EventMark {
  x: number;
  run: number;
  label: string;
}

/**
 * A count of events over one DATA frame, for when there are too many to draw
 * individually. A different claim from a mark: "this many happened somewhere in
 * here", not "one happened at this x".
 */
export interface Density {
  x0: number;
  x1: number;
  n: number;
}

export interface EventsData {
  stream: string;
  field: string;
  tier: string;
  /** Exactly one of these is populated; tier says which. */
  events: EventMark[] | null;
  density: Density[] | null;
}

export interface RecordRow {
  x: number;
  run: number;
  /**
   * What to show, and the same value as a number where one exists. Both are
   * empty for a field that was absent from the record — a guarded field whose
   * guard did not hold is not in the record at all (SPEC §6.2), and showing a
   * zero there is the bug the guard feature exists to prevent.
   */
  text: string[];
  num: (number | null)[];
}

export interface RecordsData {
  stream: string;
  fields: string[];
  rows: RecordRow[];
  offset: number;
  more: boolean;
  /** How many records the window holds; an upper bound unless totalExact. */
  total: number;
  totalExact: boolean;
  perRun: boolean;
}

export interface FrameInfo {
  offset: number;
  size: number;
  segment: number;
  stream: string;
  uuid: string;
  run: number;
  records: number;
  first: number;
  last: number;
}

export interface SegmentInfo {
  index: number;
  offset: number;
  end: number;
  schemas: number;
  runs: number;
  frames: number;
  records: number;
}

export interface FrameMapData {
  size: number;
  segments: SegmentInfo[];
  frames: FrameInfo[];
  /** How many DATA frames the file holds; `frames` may be shorter. */
  total: number;
  truncated: boolean;
}

/**
 * A signal placed on the chart stack.
 *
 * `runs` is a list because a stepped sweep is N traces sharing an axis, not one
 * trace (SPEC §6.5). One entry is a single trace; several are an overlay drawn
 * on one pane, which is how a sweep is actually read — the whole point of the
 * repeat is comparing the runs against each other. They are never merged into a
 * single series: averaging four temperature corners produces a curve the part
 * has at no temperature.
 */
export interface Signal {
  key: string;
  stream: Stream;
  field: Field;
  runs: number[];
}

export function signalKey(s: Stream, f: Field, runs: number[]): string {
  return `${s.uuid}/${f.name}/${runs.join("+")}`;
}
