// "Fell back" row: one chip per node a fallback route served instead of
// its first choice (ADR-087).
//
// Sibling of BackendsUsedRow, and deliberately NOT folded into it. That
// row answers "what ran"; this one answers "was anything degraded" —
// two backends appearing there is normal for a bot that routes work
// across them, and says nothing about whether a route was taken because
// the first one ran out. Without this, a run served by a weaker model
// is indistinguishable from a clean one after the fact: the answer is
// still well-formed, only its quality changed.
//
// Renders nothing on a clean run, so its presence always means
// something actually happened.

import type { FallbackUsage } from "@/api/runs";
import { Tooltip } from "@/components/ui";

function routeLabel(u: FallbackUsage): string {
  // A CLI backend that reports no effective model leaves `model` empty,
  // and a route the runtime could not name leaves `backend` empty too —
  // fall back to the entry name rather than rendering "undefined".
  const target = [u.backend, u.model].filter(Boolean).join(" · ");
  if (!target) return u.served_by ?? "fallback";
  return u.served_by ? `${u.served_by} (${target})` : target;
}

export default function FallbacksUsedRow({
  fallbacks,
}: {
  fallbacks: FallbackUsage[];
}) {
  if (!fallbacks || fallbacks.length === 0) return null;
  return (
    <div
      className="flex items-center gap-2 text-micro flex-wrap"
      aria-label="Fallback routes used"
    >
      <span className="shrink-0 text-warning-fg">fell back</span>
      {fallbacks.map((u) => (
        <Tooltip
          key={`${u.node_id} ${u.served_by ?? ""}`}
          content={`Node "${u.node_id}" was served by ${routeLabel(u)} after its primary failed`}
        >
          <span className="inline-flex items-center gap-1 rounded border border-warning-soft bg-warning-soft px-1.5 py-0.5 font-mono text-micro font-medium text-warning-fg">
            <span>{u.node_id}</span>
            <span aria-hidden="true">→</span>
            <span>{routeLabel(u)}</span>
          </span>
        </Tooltip>
      ))}
    </div>
  );
}
