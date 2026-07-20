import { useEffect, useState } from "preact/hooks";

import { axisToDisplay, displayToAxis, exportURL, fetchRecords } from "./api";
import type { RecordsData, Stream } from "./types";

interface Props {
  streams: Stream[];
  /** The shared view window, in display units. */
  from: number;
  to: number;
}

const PAGE = 200;

/**
 * How many records are in view, said as precisely as it is known.
 *
 * The server's total is an upper bound whenever a frame straddles the edge of
 * the window: counting exactly would mean decoding those frames just to say how
 * many, which is the work the table exists to avoid. But a single page that
 * reached the end is its own exact count — the rows are right there — so that
 * case says the number plainly instead of hedging about a bound the reader can
 * see is wrong.
 */
function count(data: RecordsData, shown: number, first: number): string {
  if (!data.more && data.offset === 0) {
    return `${shown} record${shown === 1 ? "" : "s"} in view`;
  }
  const range = shown > 0 ? `${first}–${first + shown - 1}` : "none";
  return `${range} of ${data.totalExact ? "" : "up to "}${data.total} in view`;
}

/**
 * The records themselves, which is what every chart on screen is a summary of.
 *
 * It exists to be checked against: a chart says a signal dipped here, and this
 * says what the records actually contain. That is also why an absent field is
 * an empty cell with a dash rather than a blank that could be mistaken for a
 * zero someone forgot to render.
 */
export function RecordTable({ streams, from, to }: Props) {
  const withData = streams.filter((s) => s.records > 0);
  const [uuid, setUUID] = useState(withData[0]?.uuid ?? "");
  const [page, setPage] = useState(0);
  const [data, setData] = useState<RecordsData | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const stream = withData.find((s) => s.uuid === uuid) ?? withData[0];

  // A new window or a new stream means the old page number means nothing.
  useEffect(() => setPage(0), [uuid, from, to]);

  useEffect(() => {
    if (!stream) return;
    const ac = new AbortController();
    setBusy(true);
    const a = displayToAxis(from, stream.axisKind, stream.axisExp);
    const b = displayToAxis(to, stream.axisKind, stream.axisExp);
    fetchRecords(stream.uuid, a, b, page * PAGE, PAGE, ac.signal)
      .then((d) => {
        setData(d);
        setErr(null);
      })
      .catch((e) => {
        if (e.name !== "AbortError") setErr(String(e.message ?? e));
      })
      .finally(() => setBusy(false));
    return () => ac.abort();
  }, [stream?.uuid, from, to, page]);

  if (!stream) {
    return <p class="muted pad">No stream in this file wrote a record.</p>;
  }

  const a = displayToAxis(from, stream.axisKind, stream.axisExp);
  const b = displayToAxis(to, stream.axisKind, stream.axisExp);
  const shown = data?.rows.length ?? 0;
  const first = (data?.offset ?? 0) + 1;

  return (
    <>
      <div class="drawer-tools">
        <select value={stream.uuid} onChange={(e) => setUUID((e.target as HTMLSelectElement).value)}>
          {withData.map((s) => (
            <option key={s.uuid} value={s.uuid}>
              {s.name}
            </option>
          ))}
        </select>
        {data && <span class="muted">{count(data, shown, first)}</span>}
        <span class="spacer" />
        <a class="export" href={exportURL(stream.uuid, a, b)} download>
          Export CSV
        </a>
        <button class="page" disabled={page === 0 || busy} onClick={() => setPage(page - 1)}>
          ‹
        </button>
        <button class="page" disabled={!data?.more || busy} onClick={() => setPage(page + 1)}>
          ›
        </button>
      </div>

      {err && <div class="err">{err}</div>}

      <div class="table-scroll">
        <table class="records">
          <thead>
            <tr>
              <th class="num">{stream.axisKind === "time" ? "time (s)" : stream.axisKind}</th>
              {stream.runs.length > 1 && <th class="num">run</th>}
              {data?.fields.map((f, i) => {
                const fd = stream.fields[i];
                return (
                  <th key={f} class={fd?.class === "numeric" ? "num" : ""} title={fd?.desc}>
                    {f}
                    {fd?.unit && <span class="muted"> {fd.unit}</span>}
                  </th>
                );
              })}
            </tr>
          </thead>
          <tbody>
            {data?.rows.map((row, i) => (
              <tr key={i}>
                <td class="num">
                  {axisToDisplay(row.x, stream.axisKind, stream.axisExp).toFixed(
                    stream.axisKind === "time" ? 6 : 3,
                  )}
                </td>
                {stream.runs.length > 1 && <td class="num">{row.run}</td>}
                {row.text.map((v, k) => {
                  const fd = stream.fields[k];
                  // No value at all, versus the empty string a text field can
                  // legitimately hold: the dash says the field was not in this
                  // record, and it is deliberately not a 0.
                  if (v === "" && row.num[k] == null) {
                    return (
                      <td key={k} class="absent" title="absent from this record">
                        —
                      </td>
                    );
                  }
                  return (
                    <td key={k} class={fd?.class === "numeric" ? "num" : ""}>
                      {v}
                    </td>
                  );
                })}
              </tr>
            ))}
          </tbody>
        </table>
        {data && data.rows.length === 0 && <p class="muted pad">No records in this window.</p>}
      </div>
    </>
  );
}
