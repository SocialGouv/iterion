import { useEffect, useState } from "react";
import { useLocation } from "wouter";
import { useQuery } from "@tanstack/react-query";

import type { RunSummary } from "@/api/runs";
import { listRuns } from "@/api/runs";
import { formatDate } from "@/lib/format";

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
  const [, setLocation] = useLocation();
  // Light debounce so rapid node-clicking doesn't hammer the API: the
  // query only enables once the selected node has been stable for
  // 200ms; the chip stays hidden for the newly-selected node meanwhile.
  const [debouncedNodeId, setDebouncedNodeId] = useState<string | null>(null);
  useEffect(() => {
    setDebouncedNodeId(null);
    const t = setTimeout(() => setDebouncedNodeId(nodeId), 200);
    return () => clearTimeout(t);
  }, [nodeId]);

  const query = useQuery<RunSummary[]>({
    queryKey: ["node-runs", debouncedNodeId],
    queryFn: () => listRuns({ node: debouncedNodeId!, limit: 8 }),
    enabled: debouncedNodeId !== null,
  });
  // Errors are intentionally silent (chip hidden): this is a best-effort
  // reverse-navigation decoration, not load-bearing. A runs-API outage
  // surfaces loudly in the Runs view (its own error state) —
  // double-announcing it here on every node click would be noise.
  // See docs/design-system.md § Feedback surfaces (silent-error
  // exceptions). If this ever becomes load-bearing, lift the error.
  const runs = query.error ? [] : (query.data ?? null);

  if (debouncedNodeId !== nodeId || runs === null || runs.length === 0) return null;

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
              className="w-full text-left flex items-center gap-2 px-1.5 py-1 rounded text-micro hover:bg-surface-2"
              title={`${r.status} · ${r.id}`}
            >
              <span aria-hidden className="text-fg-subtle w-3 text-center">
                {STATUS_GLYPH[r.status] ?? "?"}
              </span>
              <span className="font-mono truncate flex-1">
                {r.id.length > 20 ? `${r.id.slice(0, 16)}…` : r.id}
              </span>
              <span className="text-caption text-fg-subtle shrink-0">
                {formatDate(r.created_at)}
              </span>
            </button>
          </li>
        ))}
      </ul>
    </details>
  );
}
