import type { FileInfo, Field, Stream } from "./types";
import { signalKey } from "./types";

interface Props {
  file: FileInfo;
  selected: Set<string>;
  onToggle: (stream: Stream, field: Field, runs: number[]) => void;
}

/**
 * The signal tree: streams, their fields, and — where a stream has more than
 * one run — the runs beneath each field.
 *
 * A stepped sweep is N traces sharing an axis, not one trace (SPEC §6.5), so
 * runs are separate selectable leaves rather than being merged.
 */
export function Tree({ file, selected, onToggle }: Props) {
  return (
    <nav class="tree">
      {file.streams.map((s) => (
        <StreamNode key={s.uuid} stream={s} selected={selected} onToggle={onToggle} />
      ))}
    </nav>
  );
}

function StreamNode({ stream, selected, onToggle }: { stream: Stream } & Omit<Props, "file">) {
  return (
    <section class="stream">
      <header>
        <span class="stream-name">{stream.name}</span>
        <span class="muted">
          {stream.axisKind}/{stream.axisMode} · {stream.records.toLocaleString()} rec
        </span>
      </header>
      <ul>
        {stream.fields.map((f) => (
          <FieldNode
            key={f.name}
            stream={stream}
            field={f}
            selected={selected}
            onToggle={onToggle}
          />
        ))}
      </ul>
    </section>
  );
}

function FieldNode({
  stream,
  field,
  selected,
  onToggle,
}: { stream: Stream; field: Field } & Omit<Props, "file">) {
  const runs = stream.runs.length ? stream.runs : [{ id: 0, index: 0, label: "run 0", params: null }];
  const multiRun = runs.length > 1;

  if (!field.plottable) {
    return (
      <li class="field disabled" title={reasonNotPlottable(field)}>
        <span class="field-name">{field.name}</span>
        <Badges field={field} />
      </li>
    );
  }

  if (!multiRun) {
    const key = signalKey(stream, field, [runs[0].id]);
    return (
      <li class={`field${selected.has(key) ? " on" : ""}`}>
        <button onClick={() => onToggle(stream, field, [runs[0].id])}>
          <span class="field-name">{field.name}</span>
          <Badges field={field} />
        </button>
      </li>
    );
  }

  // Overlaying every run on one pane is only meaningful for a line chart. Two
  // state bands stacked in the same strip would occlude each other, and the
  // reading would be whichever happened to be drawn last.
  const all = runs.map((r) => r.id);
  const allKey = signalKey(stream, field, all);
  const overlayable = field.class === "numeric";

  return (
    <li class="field">
      {overlayable ? (
        <button
          class={`field-name group${selected.has(allKey) ? " on" : ""}`}
          title="Plot every run on one pane"
          onClick={() => onToggle(stream, field, all)}
        >
          {field.name}
          <span class="muted"> · {runs.length} runs</span>
        </button>
      ) : (
        <span class="field-name group">{field.name}</span>
      )}
      <Badges field={field} />
      <ul class="runs">
        {runs.map((r) => {
          const key = signalKey(stream, field, [r.id]);
          return (
            <li key={r.id} class={selected.has(key) ? "on" : ""}>
              <button onClick={() => onToggle(stream, field, [r.id])}>{r.label}</button>
            </li>
          );
        })}
      </ul>
    </li>
  );
}

function Badges({ field }: { field: Field }) {
  return (
    <span class="badges">
      {field.unit && <span class="unit">{field.unit}</span>}
      {field.class === "categorical" && <span class="badge state">state</span>}
      {/* No y value to plot, but a position and a label: it draws as a lane of
          marks rather than a line. */}
      {field.class === "event" && <span class="badge event">event</span>}
      {/* A guarded field is absent from records whose guard does not hold. The
          chart shows those stretches as gaps, so the tree says so up front. */}
      {field.guarded && <span class="badge sparse">sparse</span>}
      {field.isAxis && <span class="badge axis">axis</span>}
    </span>
  );
}

function reasonNotPlottable(f: Field): string {
  if (f.isAxis) return "carries this stream's independent variable";
  if (f.class === "blob") return `${f.type} values have no single position to draw`;
  return "";
}
