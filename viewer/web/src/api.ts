import type {
  EventsData,
  FileInfo,
  FrameMapData,
  RecordsData,
  SeriesData,
  StatesData,
} from "./types";

async function get<T>(url: string, signal?: AbortSignal): Promise<T> {
  const r = await fetch(url, { signal });
  if (!r.ok) throw new Error(`${r.status} ${r.statusText}: ${await r.text()}`);
  return r.json() as Promise<T>;
}

/** Progress of an index that is still being built. */
export interface IndexStatus {
  indexing: boolean;
  done: number;
  total: number;
  percent: number;
  error?: string;
}

/**
 * Fetches the file model, or the indexing status if it is not ready yet.
 *
 * The server comes up before indexing finishes, so 503 here is not a failure —
 * it is the honest answer that the resource exists and is not available yet.
 */
export async function fetchFileOrStatus(): Promise<
  { ready: true; file: FileInfo } | { ready: false; status: IndexStatus }
> {
  const r = await fetch("api/file");
  if (r.status === 503 || r.status === 500) {
    return { ready: false, status: (await r.json()) as IndexStatus };
  }
  if (!r.ok) throw new Error(`${r.status} ${r.statusText}: ${await r.text()}`);
  return { ready: true, file: (await r.json()) as FileInfo };
}

/**
 * Subscribes to indexing progress. Calls onDone once the index is ready, or
 * onError if it failed. Returns a function that closes the stream.
 */
export function watchProgress(
  onProgress: (s: IndexStatus) => void,
  onDone: () => void,
  onError: (message: string) => void,
): () => void {
  const es = new EventSource("api/progress");
  es.addEventListener("progress", (e) => onProgress(JSON.parse((e as MessageEvent).data)));
  es.addEventListener("ready", () => {
    es.close();
    onDone();
  });
  es.addEventListener("failed", (e) => {
    es.close();
    onError((JSON.parse((e as MessageEvent).data) as IndexStatus).error ?? "indexing failed");
  });
  return () => es.close();
}

/**
 * Fetches a series as typed arrays.
 *
 * Skips a JSON parse, halves the bytes, and lets NaN carry absence directly
 * instead of spelling it `null`. Absence is still read from the counts, not by
 * testing for NaN: a bucket with no present sample is a fact about the
 * recording, and `n === 0` states it rather than inferring it.
 */
export async function fetchSeriesBinary(
  streamUUID: string,
  field: string,
  run: number,
  from: number,
  to: number,
  points: number,
  signal?: AbortSignal,
): Promise<SeriesData & { tier: string }> {
  const q = new URLSearchParams({
    stream: streamUUID,
    field,
    run: String(run),
    from: String(from),
    to: String(to),
    points: String(points),
    format: "bin",
  });
  const r = await fetch(`api/series?${q}`, { signal });
  if (!r.ok) throw new Error(`${r.status} ${r.statusText}: ${await r.text()}`);

  const buf = await r.arrayBuffer();
  const head = new DataView(buf);
  const magic = String.fromCharCode(...new Uint8Array(buf, 0, 4));
  if (magic !== "LGBS") throw new Error(`bad series magic ${JSON.stringify(magic)}`);
  const version = head.getUint16(4, true);
  if (version !== 1) throw new Error(`unsupported series version ${version}`);
  const exact = (head.getUint8(6) & 1) !== 0;
  const n = head.getUint32(8, true);

  // 16, not 12: a Float64Array view must start on an 8-byte boundary or the
  // constructor throws. The server pads the header for exactly this reason.
  let off = 16;
  const x = new Float64Array(buf, off, n);
  off += n * 8;
  const min = new Float64Array(buf, off, n);
  off += n * 8;
  const max = new Float64Array(buf, off, n);
  off += n * 8;
  const counts = new Int32Array(buf.slice(off, off + n * 4));

  // uPlot wants nulls for gaps, so the bounds are widened to (number | null)
  // here rather than leaving NaN for the renderer to misinterpret as a value.
  const toNullable = (a: Float64Array) =>
    Array.from(a, (v, i) => (counts[i] === 0 ? null : v));

  return {
    stream: "",
    field,
    unit: "",
    run,
    exact,
    tier: r.headers.get("X-Logb-Tier") ?? "",
    x: Array.from(x),
    min: toNullable(min),
    max: toNullable(max),
    n: Array.from(counts),
  };
}

export function fetchSeries(
  streamUUID: string,
  field: string,
  run: number,
  from: number,
  to: number,
  points: number,
  signal?: AbortSignal,
): Promise<SeriesData> {
  const q = new URLSearchParams({
    stream: streamUUID,
    field,
    run: String(run),
    from: String(from),
    to: String(to),
    points: String(points),
  });
  return get<SeriesData>(`api/series?${q}`, signal);
}

export function fetchStates(
  streamUUID: string,
  field: string,
  run: number,
  from: number,
  to: number,
  points: number,
  signal?: AbortSignal,
): Promise<StatesData> {
  const q = new URLSearchParams({
    stream: streamUUID,
    field,
    run: String(run),
    from: String(from),
    to: String(to),
    points: String(points),
  });
  return get<StatesData>(`api/states?${q}`, signal);
}

/**
 * Fetches an event lane.
 *
 * No points parameter: the server decides between sending the events and
 * sending per-frame counts, and a frame is the natural bucket for the second
 * because it is what Tier 1 summarises. Asking for a pixel count would imply a
 * resolution the density path does not have.
 */
export function fetchEvents(
  streamUUID: string,
  field: string,
  run: number,
  from: number,
  to: number,
  signal?: AbortSignal,
): Promise<EventsData> {
  const q = new URLSearchParams({
    stream: streamUUID,
    field,
    run: String(run),
    from: String(from),
    to: String(to),
  });
  return get<EventsData>(`api/events?${q}`, signal);
}

/**
 * Fetches a page of decoded records.
 *
 * The window does the paging: charts and table share one range, so scrolling
 * the table is scrolling the same window the charts are showing.
 */
export function fetchRecords(
  streamUUID: string,
  from: number,
  to: number,
  offset: number,
  limit: number,
  signal?: AbortSignal,
): Promise<RecordsData> {
  const q = new URLSearchParams({
    stream: streamUUID,
    from: String(from),
    to: String(to),
    offset: String(offset),
    limit: String(limit),
  });
  return get<RecordsData>(`api/records?${q}`, signal);
}

/** Fetches the Tier 0 frame index: the file's byte layout. */
export function fetchFrames(signal?: AbortSignal): Promise<FrameMapData> {
  return get<FrameMapData>("api/frames", signal);
}

/** The CSV download URL for a window. Followed by the browser, not fetched. */
export function exportURL(streamUUID: string, from: number, to: number): string {
  const q = new URLSearchParams({
    stream: streamUUID,
    from: String(from),
    to: String(to),
  });
  return `api/export.csv?${q}`;
}

/**
 * Converts an axis value to the unit shown on screen.
 *
 * A time axis arrives as epoch-relative ticks of 10^axisExp seconds; every
 * other axis kind arrives already in its own unit and is passed through.
 */
export function axisToDisplay(v: number, axisKind: string, axisExp: number): number {
  if (axisKind !== "time") return v;
  return v * Math.pow(10, axisExp);
}

export function displayToAxis(v: number, axisKind: string, axisExp: number): number {
  if (axisKind !== "time") return v;
  return v / Math.pow(10, axisExp);
}

/** Axis label for a stream, e.g. "time (s)" or "frequency (Hz)". */
export function axisLabel(axisKind: string, axisUnit: string): string {
  const unit = axisKind === "time" ? "s" : axisUnit;
  return unit ? `${axisKind} (${unit})` : axisKind;
}
