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

// Staleness thresholds for the "stalled Ns" badge. Originally 20s
// warning / 60s danger — aggressive, given that opus / gpt-5.5 reasoning
// turns easily exceed 60s without anything being wrong. Relaxed to
// 120s / 300s in 2026-05 (finding
// `2026-05-25-runmetrics-stalled-badge-too-alarmist.md`) so the badge
// stops yelling red at 60s during normal LLM turns, while still firing
// well before the dispatcher's 10-min stall reconciliation. Exported as
// named constants for unit-testability; consumers should NOT override
// without a comparable adjustment to the tooltip copy below.
export const STALL_WARN_SECONDS = 120;
export const STALL_DANGER_SECONDS = 300;

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
  // alongside the duration value so even a multi-minute gap is visible.
  // Gate on isRunning (status === "running"), NOT the looser `active`:
  // a run parked at paused_waiting_human / paused_operator is idle on
  // purpose (waiting for a form submit or operator), not stalled. Showing
  // "stalled Ns" for it is a false alarm — the same class as the stall
  // toast suppressed in pkg/alert.
  const staleSeconds =
    m.isRunning && m.lastEventAtMs != null && nowMs > m.lastEventAtMs
      ? Math.floor((nowMs - m.lastEventAtMs) / 1000)
      : 0;
  const stalenessTone =
    staleSeconds >= STALL_DANGER_SECONDS
      ? "danger"
      : staleSeconds >= STALL_WARN_SECONDS
        ? "warning"
        : undefined;

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
          title={`No backend event in ${staleSeconds}s. Long LLM reasoning turns can take 1-3 min normally — this badge fires after ${STALL_WARN_SECONDS}s (warn) / ${STALL_DANGER_SECONDS}s (danger), well before the dispatcher's stall reconciliation kicks in (default 10 min).`}
        >
          stalled {staleSeconds}s
        </span>
      )}
      {m.costUsd > 0 && (
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
          title={budgetPillTooltip(m.budgetWarning, m.budgetExceeded)}
        >
          budget {m.budgetWarning.dimension} {Math.round(m.budgetWarning.ratio * 100)}%
        </span>
      )}
      {m.totalTokens > 0 && (
        <Metric
          label="tokens"
          // claude_code reports an aggregate without an in/out split;
          // claw fills both. Show the split when we have it, fall back
          // to the total otherwise.
          value={
            m.inputTokens > 0 || m.outputTokens > 0
              ? `${formatTokens(m.inputTokens)} / ${formatTokens(m.outputTokens)}`
              : formatTokens(m.totalTokens)
          }
          tooltip={
            m.inputTokens > 0 || m.outputTokens > 0
              ? `input ${m.inputTokens.toLocaleString()} · output ${m.outputTokens.toLocaleString()} · total ${m.totalTokens.toLocaleString()}`
              : `${m.totalTokens.toLocaleString()} tokens (aggregate; backend did not split input/output)`
          }
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
              : "All failed branches converged; nothing to jump to."
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
    <span
      className="inline-flex items-center gap-1"
      title={tooltip}
      aria-label={`${label}: ${value}`}
    >
      <span className="text-fg-subtle">{label}</span>
      <span className={`font-mono font-semibold ${valueColor}`}>
        {value}
        {live && <LiveDot tone="info" size="xs" className="ml-1 align-middle" />}
      </span>
    </span>
  );
}

// budgetPillTooltip composes the two-sentence hover hint for the budget
// warning chip. Sentence 1 names the dimension and pressure; sentence 2
// explains the consequence (hard cap reached vs. soft threshold).
// Exported for the corresponding unit test; not consumed by other
// modules.
export function budgetPillTooltip(
  warning: { dimension: string; ratio: number; used: number; limit: number },
  exceeded: boolean,
): string {
  const pct = Math.round(warning.ratio * 100);
  const head = `${warning.dimension} at ${pct}% (${warning.used} / ${warning.limit}).`;
  const tail = exceeded
    ? "Hard cap reached."
    : "Run will stop when the hard cap is hit.";
  return `${head} ${tail}`;
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
