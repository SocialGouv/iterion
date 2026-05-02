import { useMemo } from "react";

import type { RunEvent } from "@/api/runs";
import { timelineMarks } from "@/lib/snapshotReducer";

interface Props {
  events: RunEvent[];
  // Highest seq currently received from the backend (the "live" tip).
  liveSeq: number;
  // Current scrub position, or null when in live mode.
  scrubSeq: number | null;
  onChange: (next: number | null) => void;
  // Tells the scrubber to hide itself when there's nothing to scrub.
  // Keeps the run header tidy on freshly-launched runs.
  visible: boolean;
}

const MARK_COLORS: Record<string, string> = {
  run_started: "bg-info",
  run_paused: "bg-warning",
  run_resumed: "bg-info",
  run_finished: "bg-success",
  run_failed: "bg-danger",
  run_cancelled: "bg-fg-muted",
  human_input_requested: "bg-warning",
};

export default function Scrubber({
  events,
  liveSeq,
  scrubSeq,
  onChange,
  visible,
}: Props) {
  const marks = useMemo(() => timelineMarks(events), [events]);
  const isLive = scrubSeq === null;
  const value = scrubSeq ?? liveSeq;
  const max = Math.max(0, liveSeq);

  if (!visible || liveSeq <= 0) return null;

  return (
    <div className="px-4 py-1.5 border-b border-border-default flex items-center gap-3 bg-surface-1">
      <span className="text-[10px] text-fg-subtle font-mono whitespace-nowrap">
        seq
      </span>
      <div className="flex-1 relative h-5">
        <input
          type="range"
          min={0}
          max={max}
          step={1}
          value={value}
          onChange={(e) => {
            const next = Number(e.target.value);
            onChange(next === max ? null : next);
          }}
          aria-label="Time-travel scrubber"
          className="absolute inset-0 w-full h-full appearance-none bg-transparent cursor-pointer accent-accent"
        />
        {marks.length > 0 && max > 0 && (
          <div className="pointer-events-none absolute inset-x-1 top-3.5 h-1">
            {marks.map((m) => {
              const left = max === 0 ? 0 : (m.seq / max) * 100;
              return (
                <span
                  key={`${m.seq}:${m.type}`}
                  className={`absolute w-0.5 h-1.5 -translate-x-1/2 rounded ${
                    MARK_COLORS[m.type] ?? "bg-fg-subtle"
                  }`}
                  style={{ left: `${left}%` }}
                  title={`seq ${m.seq} · ${m.type}`}
                />
              );
            })}
          </div>
        )}
      </div>
      <span className="text-[10px] text-fg-subtle font-mono whitespace-nowrap">
        {value} / {max}
      </span>
      {!isLive && (
        <button
          type="button"
          onClick={() => onChange(null)}
          className="text-[10px] px-2 py-0.5 rounded bg-success-soft text-success-fg border border-success/40 hover:bg-success-soft/80"
          title="Resume live updates"
        >
          ● Live
        </button>
      )}
      {isLive && (
        <span className="text-[10px] text-success-fg flex items-center gap-1">
          <span className="w-1.5 h-1.5 rounded-full bg-success animate-pulse" />
          live
        </span>
      )}
    </div>
  );
}
