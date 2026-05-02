import { useEffect, useState } from "react";

import { useRunMetrics } from "@/hooks/useRunMetrics";
import { formatCost, formatMs, formatTokens } from "@/lib/format";

interface Props {
  // Whether the run is currently active. Drives the live duration
  // ticker — we only re-render the now() value while the run runs.
  active: boolean;
  // Optional: jump-to-failed handler wired from the parent.
  onJumpToFailed?: (nodeId: string) => void;
}

// RunMetrics renders the second line of the run header: live duration,
// cost, tokens, branch counts, jump-to-failed shortcut. Stays compact
// in a horizontal strip; collapses to "+N more" via flex-wrap on
// narrow viewports.
export default function RunMetrics({ active, onJumpToFailed }: Props) {
  const nowMs = useNow(active ? 1000 : null);
  const m = useRunMetrics(nowMs);

  return (
    <div className="px-4 py-1.5 border-b border-border-default flex flex-wrap items-center gap-x-4 gap-y-1 text-[11px] bg-surface-1">
      <Metric label="duration" value={formatMs(m.durationMs)} live={active} />
      {m.llmStepCount > 0 && (
        <Metric label="cost" value={formatCost(m.costUsd)} />
      )}
      {(m.inputTokens > 0 || m.outputTokens > 0) && (
        <Metric
          label="tokens"
          value={`${formatTokens(m.inputTokens)} / ${formatTokens(m.outputTokens)}`}
          tooltip={`input ${m.inputTokens.toLocaleString()} · output ${m.outputTokens.toLocaleString()}`}
        />
      )}
      {m.llmStepCount > 0 && (
        <Metric label="llm steps" value={String(m.llmStepCount)} />
      )}
      <Metric label="nodes" value={String(m.nodeCount)} />
      {m.branchCountActive > 0 && (
        <Metric
          label="active"
          value={String(m.branchCountActive)}
          tone="info"
        />
      )}
      {m.pausedCount > 0 && (
        <Metric
          label="paused"
          value={String(m.pausedCount)}
          tone="warning"
        />
      )}
      {m.failedCount > 0 && (
        <button
          type="button"
          disabled={!onJumpToFailed || !m.firstFailedNodeId}
          onClick={() => {
            if (m.firstFailedNodeId) onJumpToFailed?.(m.firstFailedNodeId);
          }}
          className="inline-flex items-center gap-1 text-[11px] text-danger-fg hover:underline disabled:no-underline disabled:cursor-default"
          title={
            m.firstFailedNodeId
              ? `Jump to first failed node: ${m.firstFailedNodeId}`
              : "Failed executions"
          }
        >
          <span className="text-fg-subtle">failed</span>
          <span className="font-semibold">{m.failedCount}</span>
          {onJumpToFailed && m.firstFailedNodeId && (
            <span aria-hidden>↘</span>
          )}
        </button>
      )}
    </div>
  );
}

function Metric({
  label,
  value,
  live,
  tone,
  tooltip,
}: {
  label: string;
  value: string;
  live?: boolean;
  tone?: "info" | "warning";
  tooltip?: string;
}) {
  const valueColor =
    tone === "info"
      ? "text-info-fg"
      : tone === "warning"
      ? "text-warning-fg"
      : "text-fg-default";
  return (
    <span className="inline-flex items-center gap-1" title={tooltip}>
      <span className="text-fg-subtle">{label}</span>
      <span className={`font-mono font-semibold ${valueColor}`}>
        {value}
        {live && (
          <span
            className="inline-block ml-1 w-1 h-1 rounded-full bg-info animate-pulse align-middle"
            aria-hidden
          />
        )}
      </span>
    </span>
  );
}

function useNow(intervalMs: number | null): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (intervalMs === null) {
      // Snap once when the run goes inactive so the final duration is
      // captured, then stop ticking.
      setNow(Date.now());
      return;
    }
    const id = setInterval(() => setNow(Date.now()), intervalMs);
    return () => clearInterval(id);
  }, [intervalMs]);
  return now;
}
