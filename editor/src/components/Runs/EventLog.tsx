import { useEffect, useMemo, useRef } from "react";

import type { RunEvent } from "@/api/runs";

interface Props {
  events: RunEvent[];
  selectedExecutionId: string | null;
  followTail: boolean;
  onToggleFollow: (next: boolean) => void;
}

const EVENT_BADGE: Record<string, string> = {
  run_started: "bg-info-soft text-info-fg",
  run_finished: "bg-success-soft text-success-fg",
  run_failed: "bg-danger-soft text-danger-fg",
  run_paused: "bg-warning-soft text-warning-fg",
  run_resumed: "bg-info-soft text-info-fg",
  run_cancelled: "bg-surface-2 text-fg-muted",
  node_started: "bg-info-soft text-info-fg",
  node_finished: "bg-success-soft text-success-fg",
  artifact_written: "bg-accent-soft text-fg-default",
  human_input_requested: "bg-warning-soft text-warning-fg",
  budget_warning: "bg-warning-soft text-warning-fg",
  budget_exceeded: "bg-danger-soft text-danger-fg",
};

export default function EventLog({
  events,
  selectedExecutionId,
  followTail,
  onToggleFollow,
}: Props) {
  const scrollerRef = useRef<HTMLDivElement>(null);

  const filtered = useMemo(() => {
    if (!selectedExecutionId) return events;
    // Match events by recomputing the execution ID from the event's
    // (branch, node) and the iteration count up to that seq.
    const counts = new Map<string, number>();
    return events.filter((e) => {
      if (!e.node_id) return false;
      const branch = e.branch_id || "main";
      const key = `${branch} ${e.node_id}`;
      let iter = counts.get(key);
      if (iter === undefined) iter = -1;
      if (e.type === "node_started") iter += 1;
      counts.set(key, iter);
      const id = `exec:${branch}:${e.node_id}:${iter < 0 ? 0 : iter}`;
      return id === selectedExecutionId;
    });
  }, [events, selectedExecutionId]);

  useEffect(() => {
    if (!followTail) return;
    const el = scrollerRef.current;
    if (!el) return;
    el.scrollTop = el.scrollHeight;
  }, [filtered.length, followTail]);

  return (
    <div className="h-full flex flex-col bg-surface-1">
      <div className="px-3 py-1.5 border-b border-border-default flex items-center gap-2 text-[11px]">
        <span className="font-medium text-fg-muted">Events</span>
        {selectedExecutionId && (
          <span className="text-fg-subtle">filtered by selection</span>
        )}
        <span className="text-fg-subtle">({filtered.length})</span>
        <label className="ml-auto inline-flex items-center gap-1.5 cursor-pointer">
          <input
            type="checkbox"
            checked={followTail}
            onChange={(e) => onToggleFollow(e.target.checked)}
            className="accent-accent"
          />
          Follow tail
        </label>
      </div>
      <div
        ref={scrollerRef}
        className="flex-1 overflow-auto px-3 py-1 font-mono text-[10px] leading-5"
        onScroll={(e) => {
          const el = e.currentTarget;
          const atBottom =
            el.scrollHeight - el.scrollTop - el.clientHeight < 4;
          if (!atBottom && followTail) onToggleFollow(false);
        }}
      >
        {filtered.length === 0 ? (
          <div className="text-fg-subtle py-2">No events yet.</div>
        ) : (
          filtered.map((e) => (
            <div
              key={`${e.run_id}:${e.seq}`}
              className="grid grid-cols-[auto_auto_auto_1fr] gap-2 py-0.5"
            >
              <span className="text-fg-subtle">{e.seq.toString().padStart(4, "0")}</span>
              <span
                className={`px-1.5 rounded ${EVENT_BADGE[e.type] ?? "bg-surface-2 text-fg-muted"}`}
              >
                {e.type}
              </span>
              <span className="text-fg-default truncate">{e.node_id ?? "-"}</span>
              <span className="text-fg-subtle truncate">
                {previewData(e.data)}
              </span>
            </div>
          ))
        )}
      </div>
    </div>
  );
}

function previewData(data: Record<string, unknown> | undefined): string {
  if (!data) return "";
  const interesting = ["kind", "model", "tool", "version", "publish", "to", "loop", "iteration", "error"];
  const parts: string[] = [];
  for (const k of interesting) {
    if (data[k] !== undefined) parts.push(`${k}=${formatValue(data[k])}`);
  }
  return parts.join(" ");
}

function formatValue(v: unknown): string {
  if (typeof v === "string") {
    return v.length > 60 ? v.slice(0, 57) + "…" : v;
  }
  return String(v);
}
