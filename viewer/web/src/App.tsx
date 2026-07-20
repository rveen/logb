import { useEffect, useMemo, useState } from "preact/hooks";

import { axisToDisplay, fetchFileOrStatus, watchProgress } from "./api";
import type { IndexStatus } from "./api";
import { Drawer } from "./Drawer";
import { EventLane } from "./EventLane";
import { NumericChart } from "./NumericChart";
import { StateBand } from "./StateBand";
import { Tree } from "./Tree";
import type { Field, FileInfo, Signal, Stream } from "./types";
import { signalKey } from "./types";

/**
 * The indexing screen.
 *
 * Indexing cannot be made incremental — nothing in a Logb file points forward,
 * so the whole file has to be read once — but it can be honest about how long
 * it will take.
 */
function Indexing({ status }: { status: IndexStatus }) {
  const mb = (n: number) => (n / (1024 * 1024)).toFixed(1);
  return (
    <div class="indexing">
      <h1>Indexing</h1>
      <div class="bar">
        <div class="fill" style={{ width: `${status.percent.toFixed(1)}%` }} />
      </div>
      <p class="muted">
        {status.percent.toFixed(1)}% · {mb(status.done)} of {mb(status.total)} MB
      </p>
      <p class="muted">
        Every frame has to be read once: the format never points forward, so there is no
        footer to skip to. The result is cached, and opening this file again will be
        immediate.
      </p>
    </div>
  );
}

export function App() {
  const [file, setFile] = useState<FileInfo | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [signals, setSignals] = useState<Signal[]>([]);
  const [view, setView] = useState<{ from: number; to: number } | null>(null);
  const [status, setStatus] = useState<IndexStatus | null>(null);
  const [table, setTable] = useState(false);

  useEffect(() => {
    let closeStream: (() => void) | undefined;

    const load = () => {
      fetchFileOrStatus()
        .then((r) => {
          if (r.ready) {
            setStatus(null);
            setFile(r.file);
            return;
          }
          // Still indexing. Show what progress there is and follow the stream
          // rather than polling.
          setStatus(r.status);
          if (r.status.error) {
            setErr(r.status.error);
            return;
          }
          closeStream?.();
          closeStream = watchProgress(setStatus, load, setErr);
        })
        .catch((e) => setErr(String(e.message ?? e)));
    };

    load();
    return () => closeStream?.();
  }, []);

  // The full extent of the recording, in display units, taken over every
  // stream. This is what "reset zoom" returns to and what the first chart
  // opens on.
  const extent = useMemo(() => {
    if (!file) return null;
    let lo = Infinity;
    let hi = -Infinity;
    for (const s of file.streams) {
      if (!s.hasSpan) continue;
      lo = Math.min(lo, axisToDisplay(s.axisMin, s.axisKind, s.axisExp));
      hi = Math.max(hi, axisToDisplay(s.axisMax, s.axisKind, s.axisExp));
    }
    return lo <= hi ? { from: lo, to: hi } : null;
  }, [file]);

  useEffect(() => {
    if (extent && !view) setView(extent);
  }, [extent]);

  const toggle = (stream: Stream, field: Field, runs: number[]) => {
    const key = signalKey(stream, field, runs);
    setSignals((cur) =>
      cur.some((s) => s.key === key)
        ? cur.filter((s) => s.key !== key)
        : [...cur, { key, stream, field, runs }],
    );
  };

  const onRange = (from: number, to: number) => {
    if (to > from) setView({ from, to });
  };

  if (err) return <div class="fatal">{err}</div>;
  if (status && status.indexing) return <Indexing status={status} />;
  if (!file || !view) return <div class="loading">Indexing…</div>;

  const selected = new Set(signals.map((s) => s.key));
  const zoomed = extent ? view.from > extent.from || view.to < extent.to : false;

  return (
    <div class="app">
      <header class="top">
        <div class="title">
          <strong>{file.path.split("/").pop()}</strong>
          <span class="muted">
            {(file.size / 1024).toFixed(1)} kB · {file.streams.length} streams
          </span>
        </div>
        <div class="status">
          {/* Rule 2: a file cut mid-write is a valid file containing every
              record up to the last intact frame. That is a note, not an error. */}
          {file.truncated && (
            <span class="pill warn" title="The scan stopped at damage. Every record up to that point is intact — a truncated Logb file is still a valid file.">
              truncated
            </span>
          )}
          {file.closed && (
            <span class="pill ok" title="An END frame was seen: a writer closed this file cleanly.">
              closed cleanly
            </span>
          )}
          {file.unsupported.map((u) => (
            <span key={u} class="pill warn" title={u}>
              unsupported
            </span>
          ))}
          {zoomed && (
            <button class="reset" onClick={() => extent && setView(extent)}>
              Reset zoom
            </button>
          )}
          {/* Records and the frame map: what is underneath the charts, at two
              levels. The record table reads the same window the charts do. */}
          <button class="reset" onClick={() => setTable((t) => !t)}>
            {table ? "Hide" : "Inspect"}
          </button>
        </div>
      </header>

      <div class="body">
        <aside class="side">
          <Tree file={file} selected={selected} onToggle={toggle} />
          {file.attachments.length > 0 && (
            <section class="attachments">
              <header>Attachments</header>
              <ul>
                {file.attachments.map((a) => (
                  <li key={a.name}>
                    <a href={`api/attach/${encodeURIComponent(a.name)}`} download={a.name}>
                      {a.name}
                    </a>
                    <span class="muted"> {a.size} B</span>
                  </li>
                ))}
              </ul>
            </section>
          )}
          {file.meta.length > 0 && (
            <section class="meta">
              <header>File metadata</header>
              <dl>
                {file.meta.map((m, i) => (
                  <div key={i}>
                    <dt>{m.key}</dt>
                    <dd>{m.value}</dd>
                  </div>
                ))}
              </dl>
            </section>
          )}
        </aside>

        <main class="charts">
          {signals.length === 0 && (
            <div class="empty">
              <p>Select a signal to plot.</p>
              <p class="muted">
                Drag across a chart to zoom; all charts share one axis. Fields marked{" "}
                <span class="badge sparse">sparse</span> may be absent from some records and are
                drawn with gaps rather than zeros.
              </p>
            </div>
          )}
          {signals.map((s) =>
            s.field.class === "event" ? (
              <EventLane
                key={s.key}
                signal={s}
                from={view.from}
                to={view.to}
                onRange={onRange}
                onRemove={() => toggle(s.stream, s.field, s.runs)}
              />
            ) : s.field.class === "categorical" ? (
              <StateBand
                key={s.key}
                signal={s}
                from={view.from}
                to={view.to}
                onRange={onRange}
                onRemove={() => toggle(s.stream, s.field, s.runs)}
              />
            ) : (
              <NumericChart
                key={s.key}
                signal={s}
                from={view.from}
                to={view.to}
                onRange={onRange}
                onRemove={() => toggle(s.stream, s.field, s.runs)}
              />
            ),
          )}
        </main>
      </div>

      {table && (
        <Drawer
          streams={file.streams}
          from={view.from}
          to={view.to}
          onClose={() => setTable(false)}
        />
      )}
    </div>
  );
}
