import { useEffect, useMemo, useRef, useState } from "react";

import type { RunEvent } from "@/api/runs";
import { IconButton } from "@/components/ui/IconButton";
import { LiveDot } from "@/components/ui/LiveDot";
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

// Replay speeds: how many seqs to advance per tick, paired with the
// tick period in ms. Picked to feel "fast enough not to wait, slow
// enough to read": a 5k-event run finishes in ~10s on 5×, ~2s on 25×.
// The user can hit pause + drag at any time; dragging implicitly
// pauses to keep the interaction predictable.
const REPLAY_SPEEDS: ReadonlyArray<{ label: string; step: number; tickMs: number }> = [
  { label: "1×", step: 1, tickMs: 50 },
  { label: "5×", step: 5, tickMs: 50 },
  { label: "25×", step: 25, tickMs: 50 },
];

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

  // Replay state. Lives in the Scrubber rather than RunView because
  // it's purely a UI affordance: the actual time-travel happens by
  // mutating scrubSeq (via onChange), which the rest of the app
  // already renders correctly. Pause-on-drag keeps the slider's
  // direct manipulation responsive.
  const [playing, setPlaying] = useState(false);
  const [speedIdx, setSpeedIdx] = useState(1); // default 5×
  const scrubSeqRef = useRef(scrubSeq);
  useEffect(() => {
    scrubSeqRef.current = scrubSeq;
  }, [scrubSeq]);
  useEffect(() => {
    if (!playing) return;
    const { step, tickMs } = REPLAY_SPEEDS[speedIdx]!;
    const handle = window.setInterval(() => {
      const cur = scrubSeqRef.current ?? -1;
      const next = cur + step;
      if (next >= max) {
        onChange(null); // back to live
        setPlaying(false);
        return;
      }
      onChange(next);
    }, tickMs);
    return () => window.clearInterval(handle);
  }, [playing, speedIdx, max, onChange]);

  if (!visible || liveSeq <= 0) return null;

  return (
    <div className="px-4 py-1.5 border-b border-border-default flex items-center gap-3 bg-surface-1">
      <IconButton
        size="sm"
        variant="secondary"
        label={playing ? "Pause replay" : "Play replay"}
        tooltip={playing ? "Pause replay" : (scrubSeq === null ? "Play replay from the start" : "Play replay from current position")}
        onClick={() => {
          if (playing) {
            setPlaying(false);
            return;
          }
          // Starting from live: rewind to the beginning. Starting
          // from a scrubbed position: resume from there.
          if (scrubSeq === null) onChange(0);
          setPlaying(true);
        }}
      >
        <span className="font-mono">{playing ? "⏸" : "▶"}</span>
      </IconButton>
      <select
        value={speedIdx}
        onChange={(e) => setSpeedIdx(Number(e.target.value))}
        title="Replay speed"
        className="text-[10px] px-1 py-0.5 rounded bg-surface-2 border border-border-default font-mono"
        aria-label="Replay speed"
      >
        {REPLAY_SPEEDS.map((s, i) => (
          <option key={s.label} value={i}>
            {s.label}
          </option>
        ))}
      </select>
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
            // Direct manipulation always pauses an in-progress replay
            // so the slider doesn't fight the user's drag.
            if (playing) setPlaying(false);
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
          <LiveDot tone="success" size="sm" />
          live
        </span>
      )}
    </div>
  );
}
