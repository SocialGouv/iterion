// Runs analytics view — cross-run dashboard reached from the Runs
// list toolbar.
//
// Three panels that an operator wants to see at a glance every
// morning:
//
//   1. Cost-over-time line, faceted by workflow. Answers "is iterion
//      getting more expensive over time?" and "which bot is burning
//      most spend?". Daily buckets over a configurable window.
//   2. Per-workflow runs table with status histogram, fail rate,
//      P50/P95 duration, and total cost. Answers "which bot fails
//      most?" and "which bot is slow?".
//   3. Top-line totals (run count, total cost, window). The header.
//
// Charts are rendered as inline SVG — no new dependencies, matches
// the studio's minimal-deps philosophy. The data comes from a single
// /api/v1/runs/stats call (~sub-second on hundreds of runs). The
// dashboard is a manual surface, no auto-polling — operators hit
// Refresh when they want a fresh number.

import { useCallback, useEffect, useMemo, useState } from "react";

import { getRunsStats, type StatsResponse } from "@/api/runsStats";
import { Button } from "@/components/ui/Button";
import { ErrorBoundary } from "@/components/shared/ErrorBoundary";
import { formatCost, formatMs } from "@/lib/format";

const WINDOWS = [7, 14, 30, 90] as const;
type Window = (typeof WINDOWS)[number];

export default function RunsAnalyticsView() {
  return (
    <ErrorBoundary area="Runs analytics view">
      <RunsAnalyticsViewInner />
    </ErrorBoundary>
  );
}

function RunsAnalyticsViewInner() {
  const [sinceDays, setSinceDays] = useState<Window>(30);
  const [stats, setStats] = useState<StatsResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  const refresh = useCallback(async (window: Window) => {
    setLoading(true);
    try {
      const next = await getRunsStats(window);
      setStats(next);
      setError(null);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh(sinceDays);
  }, [refresh, sinceDays]);

  const workflowColors = useMemo(
    () => assignWorkflowColors(stats?.workflows.map((w) => w.workflow) ?? []),
    [stats?.workflows],
  );

  return (
    <div className="h-full overflow-auto p-4 space-y-4 text-[13px]">
      <header className="flex items-baseline gap-3">
        <h1 className="text-lg font-semibold text-fg-default">Runs analytics</h1>
        <span className="text-fg-muted text-[11px]">
          {stats
            ? `${stats.total_runs} run${stats.total_runs === 1 ? "" : "s"} · ${formatCost(stats.total_cost_usd)} spent · last ${stats.since_days} days`
            : "loading…"}
        </span>
        <div className="ml-auto flex items-center gap-1 text-[11px]">
          {WINDOWS.map((w) => (
            <button
              key={w}
              type="button"
              className={`px-1.5 py-0.5 rounded border ${
                sinceDays === w
                  ? "border-accent bg-accent-soft text-fg-default"
                  : "border-border-default text-fg-muted hover:text-fg-default"
              }`}
              onClick={() => setSinceDays(w)}
            >
              {w}d
            </button>
          ))}
          <Button
            variant="secondary"
            size="sm"
            onClick={() => void refresh(sinceDays)}
            disabled={loading}
          >
            {loading ? "…" : "Refresh"}
          </Button>
        </div>
      </header>

      {error && (
        <div className="text-danger-fg text-[11px]" role="alert">
          {error}
        </div>
      )}

      {stats && stats.total_runs === 0 && (
        <p className="text-fg-muted text-[11px] italic">
          No runs in the last {stats.since_days} days. Launch one from{" "}
          <code className="text-[10px]">/whats-next</code> or{" "}
          <code className="text-[10px]">/editor</code> to populate this dashboard.
        </p>
      )}

      {stats && stats.total_runs > 0 && (
        <>
          <Panel title="Cost over time" subtitle="USD per day, stacked by workflow">
            <CostChart
              buckets={stats.cost_by_day}
              workflows={stats.workflows.map((w) => w.workflow)}
              colors={workflowColors}
            />
            <Legend
              entries={stats.workflows.map((w) => ({
                label: w.workflow,
                color: workflowColors[w.workflow] ?? "#888",
                value: formatCost(w.total_cost_usd),
              }))}
            />
          </Panel>

          <Panel
            title="Per-workflow stats"
            subtitle="Run count, status histogram, fail rate, P50/P95 duration, cost"
          >
            <WorkflowTable workflows={stats.workflows} colors={workflowColors} />
          </Panel>

          <Panel
            title="Duration P95"
            subtitle="Tail latency — useful for spotting workflows that crawl"
          >
            <DurationChart
              workflows={stats.workflows.filter((w) => w.duration_p95_sec > 0)}
              colors={workflowColors}
            />
          </Panel>
        </>
      )}
    </div>
  );
}

function Panel({
  title,
  subtitle,
  children,
}: {
  title: string;
  subtitle?: string;
  children: React.ReactNode;
}) {
  return (
    <section className="border border-border-subtle rounded-md bg-surface-0 p-3 space-y-2">
      <header className="flex items-baseline gap-2">
        <h2 className="text-[13px] font-medium text-fg-default">{title}</h2>
        {subtitle && (
          <span className="text-[11px] text-fg-muted">— {subtitle}</span>
        )}
      </header>
      {children}
    </section>
  );
}

// ── Cost-over-time chart ───────────────────────────────────────────────────

interface CostChartProps {
  buckets: { day: string; cost_by_workflow: Record<string, number>; total: number }[];
  workflows: string[];
  colors: Record<string, string>;
}

function CostChart({ buckets, workflows, colors }: CostChartProps) {
  if (buckets.length === 0) {
    return (
      <p className="text-[11px] text-fg-muted italic">No cost recorded in this window.</p>
    );
  }
  const W = 880;
  const H = 220;
  const PAD_LEFT = 56;
  const PAD_RIGHT = 12;
  const PAD_TOP = 12;
  const PAD_BOT = 28;
  const plotW = W - PAD_LEFT - PAD_RIGHT;
  const plotH = H - PAD_TOP - PAD_BOT;
  const max = Math.max(0.0001, ...buckets.map((b) => b.total));
  // For a stacked-bar chart per day:
  //   - X positions one tick per bucket (visible day with cost > 0).
  //   - Y goes from 0 (bottom) to max (top).
  const barW = Math.max(2, Math.min(40, plotW / Math.max(1, buckets.length) - 4));
  const xs = buckets.map((_, i) =>
    PAD_LEFT + (i + 0.5) * (plotW / Math.max(1, buckets.length)),
  );
  const yScale = (v: number) => PAD_TOP + plotH - (v / max) * plotH;
  return (
    <svg
      viewBox={`0 0 ${W} ${H}`}
      role="img"
      aria-label="Cost per day stacked by workflow"
      className="w-full h-auto"
    >
      {/* Y-axis gridlines + labels at 0%, 25%, 50%, 75%, 100% of max */}
      {[0, 0.25, 0.5, 0.75, 1].map((f) => {
        const y = PAD_TOP + plotH - f * plotH;
        return (
          <g key={f}>
            <line
              x1={PAD_LEFT}
              x2={W - PAD_RIGHT}
              y1={y}
              y2={y}
              stroke="currentColor"
              strokeOpacity={0.08}
            />
            <text
              x={PAD_LEFT - 4}
              y={y}
              textAnchor="end"
              dominantBaseline="central"
              className="fill-fg-muted"
              style={{ fontSize: "9px" }}
            >
              {formatCost(max * f)}
            </text>
          </g>
        );
      })}
      {/* Stacked bars per day */}
      {buckets.map((b, i) => {
        let yTop = yScale(b.total);
        const x = xs[i]! - barW / 2;
        const segments: React.ReactNode[] = [];
        for (const wf of workflows) {
          const v = b.cost_by_workflow[wf] ?? 0;
          if (v <= 0) continue;
          const h = (v / max) * plotH;
          segments.push(
            <rect
              key={wf}
              x={x}
              y={yTop}
              width={barW}
              height={h}
              fill={colors[wf] ?? "#888"}
              opacity={0.85}
            >
              <title>{`${b.day} — ${wf}: ${formatCost(v)}`}</title>
            </rect>,
          );
          yTop += h;
        }
        return <g key={b.day}>{segments}</g>;
      })}
      {/* X-axis labels (every Nth so they don't collide) */}
      {buckets.map((b, i) => {
        const step = Math.max(1, Math.ceil(buckets.length / 8));
        if (i % step !== 0 && i !== buckets.length - 1) return null;
        return (
          <text
            key={b.day}
            x={xs[i]}
            y={H - 10}
            textAnchor="middle"
            className="fill-fg-muted"
            style={{ fontSize: "9px" }}
          >
            {b.day.slice(5)}
          </text>
        );
      })}
    </svg>
  );
}

// ── Duration chart ──────────────────────────────────────────────────────────

interface DurationChartProps {
  workflows: {
    workflow: string;
    duration_p50_sec: number;
    duration_p95_sec: number;
  }[];
  colors: Record<string, string>;
}

function DurationChart({ workflows, colors }: DurationChartProps) {
  if (workflows.length === 0) {
    return (
      <p className="text-[11px] text-fg-muted italic">
        No finished runs in this window — duration unknown.
      </p>
    );
  }
  const max = Math.max(0.0001, ...workflows.map((w) => w.duration_p95_sec));
  return (
    <div className="space-y-2">
      {workflows.map((w) => {
        const p50Pct = (w.duration_p50_sec / max) * 100;
        const p95Pct = (w.duration_p95_sec / max) * 100;
        return (
          <div key={w.workflow} className="text-[11px]">
            <div className="flex items-baseline gap-2 mb-0.5">
              <span className="font-mono text-fg-default">{w.workflow}</span>
              <span className="text-fg-muted">
                P50 {formatMs(w.duration_p50_sec * 1000)} · P95{" "}
                {formatMs(w.duration_p95_sec * 1000)}
              </span>
            </div>
            <div
              className="relative h-3 rounded-sm bg-surface-1 overflow-hidden"
              title={`P50 ${formatMs(w.duration_p50_sec * 1000)} / P95 ${formatMs(w.duration_p95_sec * 1000)}`}
            >
              <div
                className="absolute inset-y-0 left-0"
                style={{
                  width: `${p95Pct}%`,
                  background: colors[w.workflow] ?? "#888",
                  opacity: 0.45,
                }}
              />
              <div
                className="absolute inset-y-0 left-0"
                style={{
                  width: `${p50Pct}%`,
                  background: colors[w.workflow] ?? "#888",
                }}
              />
            </div>
          </div>
        );
      })}
    </div>
  );
}

// ── Per-workflow stats table ───────────────────────────────────────────────

interface WorkflowTableProps {
  workflows: {
    workflow: string;
    run_count: number;
    fail_count: number;
    fail_rate: number;
    duration_p50_sec: number;
    duration_p95_sec: number;
    total_cost_usd: number;
    counts_by_status: Record<string, number>;
  }[];
  colors: Record<string, string>;
}

function WorkflowTable({ workflows, colors }: WorkflowTableProps) {
  const totalRuns = workflows.reduce((a, w) => a + w.run_count, 0);
  return (
    <table className="w-full text-[12px] border-collapse">
      <thead>
        <tr className="text-left text-fg-muted text-[11px]">
          <th className="font-medium py-1">Workflow</th>
          <th className="font-medium py-1 text-right">Runs</th>
          <th className="font-medium py-1">Status</th>
          <th className="font-medium py-1 text-right">Fail rate</th>
          <th className="font-medium py-1 text-right">P50 / P95</th>
          <th className="font-medium py-1 text-right">Cost</th>
        </tr>
      </thead>
      <tbody>
        {workflows.map((w) => {
          const rateClass =
            w.fail_rate >= 0.5
              ? "text-danger-fg"
              : w.fail_rate >= 0.2
                ? "text-warning-fg"
                : "text-fg-default";
          return (
            <tr key={w.workflow} className="border-t border-border-subtle">
              <td className="py-1">
                <span
                  className="inline-block w-2 h-2 rounded-full mr-2 align-middle"
                  style={{ background: colors[w.workflow] ?? "#888" }}
                />
                <span className="font-mono text-fg-default">{w.workflow}</span>
              </td>
              <td className="py-1 text-right tabular-nums text-fg-default">
                {w.run_count}
                <span className="text-fg-subtle text-[10px] ml-1">
                  ({totalRuns > 0 ? Math.round((w.run_count / totalRuns) * 100) : 0}%)
                </span>
              </td>
              <td className="py-1">
                <StatusHistogram counts={w.counts_by_status} total={w.run_count} />
              </td>
              <td className={`py-1 text-right tabular-nums ${rateClass}`}>
                {(w.fail_rate * 100).toFixed(0)}%
                <span className="text-fg-subtle text-[10px] ml-1">
                  ({w.fail_count}/{w.run_count})
                </span>
              </td>
              <td className="py-1 text-right tabular-nums text-fg-muted">
                {formatMs(w.duration_p50_sec * 1000)} /{" "}
                <span className="text-fg-default">
                  {formatMs(w.duration_p95_sec * 1000)}
                </span>
              </td>
              <td className="py-1 text-right tabular-nums text-fg-default">
                {formatCost(w.total_cost_usd)}
              </td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}

// StatusHistogram renders a compact horizontal bar split by status,
// so the operator sees finished/failed/cancelled at a glance without
// scanning numbers. Each segment carries a title for the per-status
// count.
function StatusHistogram({
  counts,
  total,
}: {
  counts: Record<string, number>;
  total: number;
}) {
  if (total === 0) return null;
  const order = [
    "finished",
    "running",
    "paused_waiting_human",
    "queued",
    "failed_resumable",
    "failed",
    "cancelled",
  ];
  const entries = order
    .map((s) => ({ status: s, n: counts[s] ?? 0 }))
    .filter((e) => e.n > 0);
  return (
    <div className="inline-flex h-3 w-32 rounded-sm overflow-hidden bg-surface-1">
      {entries.map((e) => (
        <div
          key={e.status}
          style={{
            width: `${(e.n / total) * 100}%`,
            background: statusColor(e.status),
          }}
          title={`${e.status}: ${e.n}`}
        />
      ))}
    </div>
  );
}

function statusColor(status: string): string {
  switch (status) {
    case "finished":
      return "#22c55e"; // emerald
    case "running":
      return "#3b82f6"; // blue
    case "paused_waiting_human":
      return "#a855f7"; // violet
    case "queued":
      return "#64748b"; // slate
    case "failed_resumable":
      return "#f59e0b"; // amber
    case "failed":
      return "#ef4444"; // red
    case "cancelled":
      return "#94a3b8"; // slate-light
    default:
      return "#64748b";
  }
}

// ── Legend ──────────────────────────────────────────────────────────────────

function Legend({
  entries,
}: {
  entries: { label: string; color: string; value: string }[];
}) {
  return (
    <div className="flex flex-wrap gap-3 text-[11px]">
      {entries.map((e) => (
        <span key={e.label} className="inline-flex items-center gap-1">
          <span
            className="w-2.5 h-2.5 rounded-sm"
            style={{ background: e.color }}
          />
          <span className="font-mono text-fg-default">{e.label}</span>
          <span className="text-fg-muted">{e.value}</span>
        </span>
      ))}
    </div>
  );
}

// assignWorkflowColors maps each workflow name to one of a fixed
// palette in stable order — so a workflow keeps its color across
// re-renders and across different windows (same workflow ≡ same hue
// in the chart, table, and legend).
const PALETTE = [
  "#3b82f6", // blue
  "#a855f7", // violet
  "#22c55e", // emerald
  "#f59e0b", // amber
  "#ec4899", // pink
  "#06b6d4", // cyan
  "#84cc16", // lime
  "#f43f5e", // rose
  "#0ea5e9", // sky
];

function assignWorkflowColors(workflows: string[]): Record<string, string> {
  const out: Record<string, string> = {};
  workflows.forEach((wf, i) => {
    out[wf] = PALETTE[i % PALETTE.length] ?? "#888";
  });
  return out;
}
