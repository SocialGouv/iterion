import { useEffect, useState } from "react";

import { useRunMetrics } from "@/hooks/useRunMetrics";
import { LiveDot } from "@/components/ui/LiveDot";
import { formatCost, formatMs, formatTokens } from "@/lib/format";

interface Props {
  // Whether the run is currently active. Drives the live duration
  // ticker — we only re-render the now() value while the run runs.
  active: boolean;
  // Optional: jump-to-failed handler wired from the parent.
  onJumpToFailed?: (nodeId: string) => void;
  // When true, strip the outer border + bg so the caller can fuse
  // RunMetrics with another panel (e.g. inline with the Scrubber).
  bare?: boolean;
}

// RunMetrics renders the second line of the run header: live duration,
// cost, tokens, branch counts, jump-to-failed shortcut. Stays compact
// in a horizontal strip; collapses to "+N more" via flex-wrap on
// narrow viewports.
export default function RunMetrics({ active, onJumpToFailed, bare = false }: Props) {
  const nowMs = useNow(active ? 1000 : null);
  const m = useRunMetrics(nowMs);

  // Staleness — surface the silent-stuck case the 2026-05-21 internet
  // outage exposed. When a run is `running` but the backend has stopped
  // emitting events (lost connection, subprocess hung), nothing on the
  // page changes; the operator only learns about it when stall
  // reconciliation kicks in 10min later. This badge ticks every second
  // alongside the duration value, so even a 30s gap is visible.
  const staleSeconds =
    active && m.lastEventAtMs != null && nowMs > m.lastEventAtMs
      ? Math.floor((nowMs - m.lastEventAtMs) / 1000)
      : 0;
  const stalenessTone =
    staleSeconds >= 60 ? "danger" : staleSeconds >= 20 ? "warning" : undefined;

  const outerClass = bare
    ? "h-full px-4 py-1.5 flex flex-wrap items-center gap-x-4 gap-y-1 text-[11px]"
    : "px-4 py-1.5 border-b border-border-default flex flex-wrap items-center gap-x-4 gap-y-1 text-[11px] bg-surface-1";
  return (
    <div className={outerClass}>
      <Metric label="duration" value={formatMs(m.durationMs)} live={active} />
      {stalenessTone && (
        <span
          className={`inline-flex items-center gap-1 px-1.5 py-0.5 rounded text-[10px] border ${
            stalenessTone === "danger"
              ? "bg-danger-soft text-danger-fg border-danger/40"
              : "bg-warning-soft text-warning-fg border-warning/40"
          }`}
          title={`No backend event in ${staleSeconds}s. The run might be waiting on a slow LLM response, the network may be down, or the subprocess may be hung. Stall reconciliation fires at 10 min.`}
        >
          stalled {staleSeconds}s
        </span>
      )}
      {m.llmStepCount > 0 && (
        <Metric
          label="cost"
          value={formatCost(m.costUsd)}
          tone={
            m.budgetExceeded
              ? "danger"
              : m.budgetWarning?.dimension === "cost_usd"
              ? "warning"
              : undefined
          }
          tooltip={
            m.budgetWarning?.dimension === "cost_usd"
              ? `Budget warning: ${Math.round(m.budgetWarning.ratio * 100)}% of $${m.budgetWarning.limit.toFixed(2)} consumed.`
              : undefined
          }
        />
      )}
      {m.budgetWarning && (
        <span
          className={`inline-flex items-center gap-1 px-1.5 py-0.5 rounded text-[10px] border ${
            m.budgetExceeded
              ? "bg-danger-soft text-danger-fg border-danger/40"
              : "bg-warning-soft text-warning-fg border-warning/40"
          }`}
          title={`The runtime hit ${Math.round(m.budgetWarning.ratio * 100)}% of the ${m.budgetWarning.dimension} budget (${m.budgetWarning.used} / ${m.budgetWarning.limit}). ${
            m.budgetExceeded ? "Hard cap reached." : "Soft threshold; the run will be cancelled once the hard cap is hit."
          }`}
        >
          budget {m.budgetWarning.dimension} {Math.round(m.budgetWarning.ratio * 100)}%
        </span>
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
  tone?: "info" | "warning" | "danger";
  tooltip?: string;
}) {
  const valueColor =
    tone === "info"
      ? "text-info-fg"
      : tone === "warning"
      ? "text-warning-fg"
      : tone === "danger"
      ? "text-danger-fg"
      : "text-fg-default";
  return (
    <span className="inline-flex items-center gap-1" title={tooltip}>
      <span className="text-fg-subtle">{label}</span>
      <span className={`font-mono font-semibold ${valueColor}`}>
        {value}
        {live && <LiveDot tone="info" size="xs" className="ml-1 align-middle" />}
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
