import { useState } from "preact/hooks";

import { FrameMap } from "./FrameMap";
import { RecordTable } from "./RecordTable";
import type { Stream } from "./types";

interface Props {
  streams: Stream[];
  /** The shared view window, in display units. */
  from: number;
  to: number;
  onClose: () => void;
}

type Tab = "records" | "frames";

/**
 * The bottom drawer: what is underneath the charts.
 *
 * Two views of the same file at two levels. Records are what the charts are a
 * summary of, over the same window. Frames are where those records physically
 * live — the layout that makes random access, decimation and resynchronisation
 * work at all.
 */
export function Drawer({ streams, from, to, onClose }: Props) {
  const [tab, setTab] = useState<Tab>("records");

  return (
    <div class="drawer">
      <div class="drawer-head">
        <button class={`tab${tab === "records" ? " on" : ""}`} onClick={() => setTab("records")}>
          Records
        </button>
        <button class={`tab${tab === "frames" ? " on" : ""}`} onClick={() => setTab("frames")}>
          Frame map
        </button>
        <span class="spacer" />
        <button class="close" onClick={onClose} title="Close">
          ×
        </button>
      </div>
      {tab === "records" ? (
        <RecordTable streams={streams} from={from} to={to} />
      ) : (
        <FrameMap streams={streams} />
      )}
    </div>
  );
}
