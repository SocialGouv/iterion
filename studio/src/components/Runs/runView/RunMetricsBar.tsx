import { type RunEvent } from "@/api/runs";

import RunMetrics from "../RunMetrics";
import Scrubber from "../Scrubber";

// The thin strip under RunHeader that pairs RunMetrics (cost/llm/tokens
// pills) with the time-travel Scrubber. Both sit in `bare` mode so the
// strip can compose them in a single flex row without each carrying its
// own border. The Scrubber is hidden until events arrive (liveSeq > 0)
// so a freshly-launched run doesn't paint an empty timeline.
//
// Pure presentational — lifted out of RunView so the host's render tree
// stays focused on the panel split / file dialogs.
export function RunMetricsBar({
  active,
  events,
  liveSeq,
  scrubSeq,
  onScrubChange,
  onJumpToFailed,
  bare = false,
}: {
  active: boolean;
  events: RunEvent[];
  liveSeq: number;
  scrubSeq: number | null;
  onScrubChange: (next: number | null) => void;
  onJumpToFailed: (nodeId: string) => void;
  // When true, drop the strip's own bottom border so a parent can fuse it
  // into a single bordered header block with RunHeader. The subtle
  // surface-1 band (the "raised vitals ribbon" look) is kept.
  bare?: boolean;
}) {
  return (
    <div
      className={`bg-surface-1 flex items-stretch ${
        bare ? "" : "border-b border-border-default"
      }`}
    >
      <div className="flex-shrink-0">
        <RunMetrics active={active} onJumpToFailed={onJumpToFailed} bare />
      </div>
      {liveSeq > 0 && (
        <div className="flex-1 min-w-0 border-l border-border-default">
          <Scrubber
            events={events}
            liveSeq={liveSeq}
            scrubSeq={scrubSeq}
            onChange={onScrubChange}
            visible
            bare
          />
        </div>
      )}
    </div>
  );
}
