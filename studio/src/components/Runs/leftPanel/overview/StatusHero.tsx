import { useMemo } from "react";

import { LiveDot, Stat, StatusBadge } from "@/components/ui";
import { NodeIcon } from "@/components/icons/NodeIcon";
import { formatMs } from "@/lib/format";
import type { NodeKind } from "@/api/types";
import type { ExecutionState, RunHeader } from "@/api/runs";
import { useNodeLabel } from "@/lib/runChat/useNodeLabel";

interface StatusHeroProps {
  run: RunHeader;
  executions: ExecutionState[];
  nowMs: number;
  /** Live active-duration ms from the shared run metrics. */
  durationMs: number;
}

// StatusHero is the Overview's opening card — the run's "how's it going
// right now?" at a glance: the status, a live heartbeat + the node
// currently executing while running, or a one-line reason while paused /
// failed / queued. It's intentionally INFORMATIONAL — the run's actions
// (Pause / Cancel / Resume / Answer) live in the header, which stays the
// single action surface; the hero points the operator there.
export function StatusHero({
  run,
  executions,
  nowMs,
  durationMs,
}: StatusHeroProps) {
  const isRunning = run.status === "running";

  // The node currently executing (earliest-started running exec). Small
  // set, but memoise so the per-second nowMs tick doesn't re-scan it.
  const running = useMemo(() => {
    const active = executions
      .filter((e) => e.status === "running" && e.started_at)
      .sort((a, b) => (a.started_at! < b.started_at! ? -1 : 1));
    return { first: active[0], extra: Math.max(0, active.length - 1) };
  }, [executions]);

  return (
    <div className="rounded-[var(--radius-lg)] border border-border-default bg-surface-2/50 px-3 py-2.5 space-y-2">
      <div className="flex items-center gap-2 flex-wrap">
        <StatusBadge status={run.status} size="md" />
        {isRunning && <LiveDot tone="live" size="sm" label="Run active" />}
        {durationMs > 0 && (
          <div className="ml-auto">
            <Stat
              label={isRunning ? "active" : "took"}
              value={formatMs(durationMs)}
              live={isRunning}
              hint={
                isRunning
                  ? "Active wall-clock so far (excludes paused / failed-resumable gaps)."
                  : "Total active wall-clock the run consumed."
              }
            />
          </div>
        )}
      </div>

      <ActivityLine run={run} running={running} nowMs={nowMs} />
    </div>
  );
}

function ActivityLine({
  run,
  running,
  nowMs,
}: {
  run: RunHeader;
  running: { first?: ExecutionState; extra: number };
  nowMs: number;
}) {
  const nodeLabel = useNodeLabel();
  switch (run.status) {
    case "running": {
      const ex = running.first;
      if (!ex) {
        return <span className="text-caption text-fg-subtle">running…</span>;
      }
      const startedMs = ex.started_at ? Date.parse(ex.started_at) : NaN;
      const elapsed = Number.isFinite(startedMs)
        ? Math.max(0, nowMs - startedMs)
        : null;
      return (
        <div className="flex items-center gap-1.5 text-caption min-w-0">
          {ex.kind && (
            <NodeIcon kind={ex.kind as NodeKind} size={14} className="shrink-0" />
          )}
          <span className="text-fg-default truncate" title={ex.ir_node_id}>
            {nodeLabel(ex.ir_node_id)}
          </span>
          {elapsed !== null && (
            <span className="text-fg-subtle shrink-0">· {formatMs(elapsed)}</span>
          )}
          {running.extra > 0 && (
            <span className="text-fg-subtle shrink-0">
              +{running.extra} more
            </span>
          )}
        </div>
      );
    }
    case "paused_waiting_human":
      return (
        <div className="text-caption text-info-fg">
          Waiting for your input — answer in the conversation panel.
        </div>
      );
    case "paused_operator": {
      const node = run.checkpoint?.node_id;
      return (
        <div className="text-caption text-warning-fg">
          Halted{node ? ` before ${node}` : ""} — resume from the header.
        </div>
      );
    }
    case "failed":
    case "failed_resumable": {
      if (!run.error) return null;
      const firstLine = run.error.split("\n")[0] ?? run.error;
      return (
        <div
          className="text-caption text-danger-fg line-clamp-2"
          title={run.error}
        >
          {firstLine}
        </div>
      );
    }
    case "queued":
      return (
        <div className="text-caption text-fg-subtle">
          {typeof run.queue_position === "number"
            ? `#${run.queue_position} in queue`
            : "queued"}
        </div>
      );
    default:
      return null;
  }
}
