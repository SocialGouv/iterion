import { useEffect, useMemo, useRef, useState } from "react";

import type { RunEvent } from "@/api/runs";
import { IconButton } from "@/components/ui/IconButton";
import { LiveDot } from "@/components/ui/LiveDot";
import { Select } from "@/components/ui/Select";
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
  // When true, strip the outer border + bg so the caller can fuse
  // the Scrubber with the RunMetrics row.
  bare?: boolean;
}

// Replay speeds are TRUE wall-clock multipliers: playback waits the real
// inter-event gap divided by the multiplier, so ×5 replays a 10-minute
// run in ~2 minutes. Idle gaps whose *playback* wait would exceed
// GAP_CAP_MS (human pauses, long LLM turns) are compressed to the cap
// and surfaced via the ⏩ indicator — ×1 stays watchable without lying
// about the pace of the busy sections. "Instant" keeps the old
// fixed-seq-stepping behaviour for skimming a run's shape.
const REPLAY_SPEEDS: ReadonlyArray<{ label: string; mult: number }> = [
  { label: "×1", mult: 1 },
  { label: "×2", mult: 2 },
  { label: "×5", mult: 5 },
  { label: "×10", mult: 10 },
  { label: "×25", mult: 25 },
  { label: "Instant", mult: 0 },
];
// Max playback wait for a single inter-event gap.
const GAP_CAP_MS = 3_000;
// Hops shorter than ~a frame are coalesced into one slider advance so
// log bursts don't schedule hundreds of near-zero timeouts.
const BATCH_MS = 16;
// Fallback hop when an event has no parsable timestamp.
const NO_TS_HOP_MS = 50;
// Legacy "Instant" pace (the old 25× seq-stepping).
const INSTANT_STEP = 25;
const INSTANT_TICK_MS = 50;
// Only surface gap compression worth noticing (in real run time).
const SKIP_NOTE_MIN_MS = 5_000;

const MARK_COLORS: Record<string, string> = {
  run_started: "bg-info",
  run_paused: "bg-warning",
  run_resumed: "bg-info",
  run_finished: "bg-success",
  run_failed: "bg-danger",
  run_cancelled: "bg-fg-muted",
  human_input_requested: "bg-warning",
};

function fmtClock(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000));
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  const ss = String(sec).padStart(2, "0");
  if (h > 0) return `${h}:${String(m).padStart(2, "0")}:${ss}`;
  return `${m}:${ss}`;
}

export default function Scrubber({
  events,
  liveSeq,
  scrubSeq,
  onChange,
  visible,
  bare = false,
}: Props) {
  const marks = useMemo(() => timelineMarks(events), [events]);
  const isLive = scrubSeq === null;
  const value = scrubSeq ?? liveSeq;
  const max = Math.max(0, liveSeq);

  // seq→timestamp walk table. Events arrive seq-ordered from the
  // stream; the defensive sort keeps the walker correct on refetches.
  const timeline = useMemo(() => {
    const rows = events.map((e) => {
      const ts = Date.parse(e.timestamp);
      return { seq: e.seq, ts: Number.isFinite(ts) ? ts : NaN };
    });
    rows.sort((a, b) => a.seq - b.seq);
    return rows;
  }, [events]);

  // First/last known timestamps — power the T+elapsed / total display.
  const bounds = useMemo(() => {
    let first = NaN;
    let last = NaN;
    for (const r of timeline) {
      if (!Number.isFinite(r.ts)) continue;
      if (!Number.isFinite(first)) first = r.ts;
      last = r.ts;
    }
    return Number.isFinite(first) ? { first, last } : null;
  }, [timeline]);

  const elapsedMs = useMemo(() => {
    if (!bounds) return null;
    let cur = bounds.first;
    for (const r of timeline) {
      if (r.seq > value) break;
      if (Number.isFinite(r.ts)) cur = r.ts;
    }
    return cur - bounds.first;
  }, [bounds, timeline, value]);

  // Replay state. Lives in the Scrubber rather than RunView because
  // it's purely a UI affordance: the actual time-travel happens by
  // mutating scrubSeq (via onChange), which the rest of the app
  // already renders correctly. Pause-on-drag keeps the slider's
  // direct manipulation responsive.
  const [playing, setPlaying] = useState(false);
  const [speedIdx, setSpeedIdx] = useState(2); // default ×5
  // Transient "⏩ +2m14s" note when idle gaps get compressed.
  const [skipNote, setSkipNote] = useState<{ ms: number; key: number } | null>(null);

  // The playback loop reads everything through a ref so speed changes
  // and live event growth never restart an in-flight timer. Updated in
  // an effect (declared before the loop effect, so mount ordering keeps
  // it fresh) rather than during render.
  const stateRef = useRef({ scrubSeq, max, timeline, mult: 1, onChange });
  useEffect(() => {
    stateRef.current = {
      scrubSeq,
      max,
      timeline,
      mult: REPLAY_SPEEDS[speedIdx]?.mult ?? 1,
      onChange,
    };
  }, [scrubSeq, max, timeline, speedIdx, onChange]);

  useEffect(() => {
    if (!playing) return;
    let cancelled = false;
    let timer: number | null = null;

    const finish = () => {
      stateRef.current.onChange(null); // back to live
      setPlaying(false);
    };

    // `curOverride` threads the position along the timeout chain: the
    // React state behind stateRef only syncs after a re-render, so the
    // continuation right after onChange(target) would otherwise read a
    // stale position and re-schedule (= double) every hop.
    const tick = (curOverride?: number) => {
      if (cancelled) return;
      const { scrubSeq, max, timeline, mult, onChange } = stateRef.current;
      const cur = curOverride ?? scrubSeq ?? -1;

      if (mult === 0) {
        // Instant: legacy fixed seq stepping.
        const next = cur + INSTANT_STEP;
        if (next >= max) {
          finish();
          return;
        }
        onChange(next);
        timer = window.setTimeout(() => tick(next), INSTANT_TICK_MS);
        return;
      }

      // Lower-bound binary search: first timeline row with seq > cur.
      let lo = 0;
      let hi = timeline.length;
      while (lo < hi) {
        const mid = (lo + hi) >> 1;
        const row = timeline[mid];
        if (row === undefined || row.seq <= cur) lo = mid + 1;
        else hi = mid;
      }
      let i = lo;
      const head = timeline[i];
      if (head === undefined || head.seq >= max) {
        finish();
        return;
      }

      // Baseline timestamp: last known ts at or before cur.
      let prevTs = NaN;
      for (let j = i - 1; j >= 0; j--) {
        const row = timeline[j];
        if (row !== undefined && Number.isFinite(row.ts)) {
          prevTs = row.ts;
          break;
        }
      }

      // Coalesce sub-frame hops; a gap that stands on its own gets its
      // own timeout so burst events surface BEFORE the gap, not after.
      // `delay` is playback wait, `skipped` is real run time dropped by
      // the gap cap.
      let delay = 0;
      let skipped = 0;
      let target = cur;
      for (;;) {
        const row = timeline[i];
        if (row === undefined || row.seq >= max) break;
        const real =
          Number.isFinite(row.ts) && Number.isFinite(prevTs)
            ? Math.max(0, row.ts - prevTs)
            : NaN;
        const scaled = Number.isFinite(real) ? real / mult : NO_TS_HOP_MS;
        const capped = Math.min(scaled, GAP_CAP_MS);
        // Flush the accumulated batch before starting a standalone gap.
        if (delay > 0 && delay + capped > BATCH_MS) break;
        if (Number.isFinite(real)) {
          skipped += Math.max(0, real - capped * mult);
          prevTs = row.ts;
        }
        delay += capped;
        target = row.seq;
        i++;
        if (delay > BATCH_MS) break;
      }

      if (skipped >= SKIP_NOTE_MIN_MS) {
        setSkipNote((prev) => ({
          ms: (prev?.ms ?? 0) + skipped,
          key: (prev?.key ?? 0) + 1,
        }));
      }

      timer = window.setTimeout(() => {
        if (cancelled) return;
        stateRef.current.onChange(target);
        tick(target);
      }, delay);
    };

    tick();
    return () => {
      cancelled = true;
      if (timer !== null) window.clearTimeout(timer);
    };
  }, [playing]);

  // The ⏩ note fades on its own shortly after the last compression.
  useEffect(() => {
    if (!skipNote) return;
    const h = window.setTimeout(() => setSkipNote(null), 2500);
    return () => window.clearTimeout(h);
  }, [skipNote]);

  if (!visible || liveSeq <= 0) return null;

  const outerClass = bare
    ? "h-full px-4 py-1.5 flex items-center gap-3"
    : "px-4 py-1.5 border-b border-border-default flex items-center gap-3 bg-surface-1";
  return (
    <div className={outerClass}>
      <IconButton
        size="sm"
        variant="secondary"
        label={
          playing
            ? "Pause replay"
            : scrubSeq === null
              ? "Play replay from the start"
              : "Play replay from current position"
        }
        tooltip={
          playing
            ? "Pause replay"
            : scrubSeq === null
              ? "Play replay from the start"
              : "Play replay from current position"
        }
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
      <Select
        size="sm"
        fit
        value={speedIdx}
        onChange={(e) => setSpeedIdx(Number(e.target.value))}
        title="Replay speed (wall-clock multiplier)"
        aria-label="Replay speed"
        className="font-mono"
      >
        {REPLAY_SPEEDS.map((s, i) => (
          <option key={s.label} value={i}>
            {s.label}
          </option>
        ))}
      </Select>
      {skipNote && (
        <span
          className="text-caption text-warning-fg font-mono whitespace-nowrap"
          title="Idle gap compressed — real run time skipped by the replay"
        >
          ⏩ +{fmtClock(skipNote.ms)}
        </span>
      )}
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
      {bounds && elapsedMs !== null && (
        <span
          className="text-caption text-fg-subtle font-mono whitespace-nowrap"
          title="Run time at the current position / total run time"
        >
          T+{fmtClock(elapsedMs)} / {fmtClock(bounds.last - bounds.first)}
        </span>
      )}
      <span className="text-caption text-fg-subtle font-mono whitespace-nowrap">
        {value} / {max}
      </span>
      {!isLive && (
        <button
          type="button"
          onClick={() => onChange(null)}
          className="text-caption px-2 py-0.5 rounded bg-success-soft text-success-fg border border-success/40 hover:bg-success-soft/80"
          title="Stop replay and follow live events again."
        >
          ● Live
        </button>
      )}
      {isLive && (
        <span className="text-caption text-success-fg flex items-center gap-1">
          <LiveDot tone="success" size="sm" />
          live
        </span>
      )}
    </div>
  );
}
