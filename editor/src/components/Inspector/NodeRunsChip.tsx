import { useEffect, useState } from "react";
import { useLocation } from "wouter";

import type { RunSummary } from "@/api/runs";
import { listRuns } from "@/api/runs";

interface Props {
  nodeId: string;
}

const STATUS_GLYPH: Record<string, string> = {
  finished: "✓",
  failed: "✗",
  failed_resumable: "↻",
  cancelled: "⊘",
  running: "•",
  paused_waiting_human: "⏸",
};

/** Reverse-navigation surface: shows "this node was touched by N runs"
 *  on the currently-selected workflow node and lets the user jump back
 *  to any of them. Lives in the Inspector (below the form) so the
 *  canvas stays uncluttered — the chip only appears when the user
 *  has actually engaged with the node. */
export default function NodeRunsChip({ nodeId }: Props) {
  const [runs, setRuns] = useState<RunSummary[] | null>(null);
  const [, setLocation] = useLocation();

  useEffect(() => {
    setRuns(null);
    let cancelled = false;
    // Light debounce so rapid node-clicking doesn't hammer the API.
    const t = setTimeout(() => {
      listRuns({ node: nodeId, limit: 8 })
        .then((rs) => {
          if (cancelled) return;
          setRuns(rs);
        })
        .catch(() => {
          // Silent fall-through: a runs API outage shouldn't break the
          // editor; just hide the chip.
          if (!cancelled) setRuns([]);
        });
    }, 200);
    return () => {
      cancelled = true;
      clearTimeout(t);
    };
  }, [nodeId]);

  if (runs === null || runs.length === 0) return null;

  return (
    <details
      className="mx-3 mb-2 rounded border border-border-default bg-surface-1"
      open={runs.length <= 3}
    >
      <summary className="cursor-pointer px-2 py-1.5 text-xs text-fg-default flex items-center gap-1.5">
        <span aria-hidden>↻</span>
        <span>
          {runs.length} run{runs.length > 1 ? "s" : ""} touched this node
        </span>
      </summary>
      <ul className="px-2 pb-2 space-y-0.5">
        {runs.map((r) => (
          <li key={r.id}>
            <button
              type="button"
              onClick={() => setLocation(`/runs/${encodeURIComponent(r.id)}`)}
              className="w-full text-left flex items-center gap-2 px-1.5 py-1 rounded text-[11px] hover:bg-surface-2"
              title={`${r.status} · ${r.id}`}
            >
              <span aria-hidden className="text-fg-subtle w-3 text-center">
                {STATUS_GLYPH[r.status] ?? "?"}
              </span>
              <span className="font-mono truncate flex-1">
                {r.id.length > 20 ? `${r.id.slice(0, 16)}…` : r.id}
              </span>
              <span className="text-[10px] text-fg-subtle shrink-0">
                {new Date(r.created_at).toLocaleDateString()}
              </span>
            </button>
          </li>
        ))}
      </ul>
    </details>
  );
}
