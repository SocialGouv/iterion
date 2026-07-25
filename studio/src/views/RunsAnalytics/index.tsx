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

import { useMemo, useState } from "react";
import { Link } from "wouter";
import { ArrowLeftIcon } from "@radix-ui/react-icons";
import { keepPreviousData, useQuery } from "@tanstack/react-query";

import { getRunsStats, type StatsResponse } from "@/api/runsStats";
import { Button } from "@/components/ui/Button";
import { EmptyState } from "@/components/ui/EmptyState";
import { Skeleton } from "@/components/ui/Skeleton";
import { Table, THead, Th, TBody, Tr, Td } from "@/components/ui/Table";
import { ErrorBoundary } from "@/components/shared/ErrorBoundary";
import { errorMessage } from "@/lib/errorHints";
import { formatCost, formatMs } from "@/lib/format";
import { useServerInfoStore } from "@/store/serverInfo";

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
  const cloud = useServerInfoStore((s) => s.info?.mode === "cloud");

  // keepPreviousData: switching the window keeps the current dashboard
  // on screen while the new one loads — the skeleton is first-load only.
  const statsQuery = useQuery<StatsResponse>({
    queryKey: ["runs-stats", sinceDays],
    queryFn: () => getRunsStats(sinceDays),
    placeholderData: keepPreviousData,
  });
  const stats = statsQuery.data ?? null;
  const loading = statsQuery.isFetching;
  const error =
    statsQuery.error && !loading ? errorMessage(statsQuery.error) : null;

  const workflowColors = useMemo(
    () => assignWorkflowColors(stats?.workflows.map((w) => w.workflow) ?? []),
    [stats?.workflows],
  );

  return (
    <div className="h-full overflow-auto p-4 space-y-4 text-label">
      <header className="flex items-baseline gap-3">
        <Link
          href="/runs"
          className="text-fg-muted hover:text-fg-default text-micro inline-flex items-center gap-1 shrink-0"
        >
          <ArrowLeftIcon className="w-3 h-3" />
          Runs
        </Link>
        <h1 className="text-headline font-semibold tracking-tight text-fg-default">
          Runs analytics
        </h1>
        <span className="text-fg-muted text-micro">
          {stats
            ? `${stats.total_runs} run${stats.total_runs === 1 ? "" : "s"} · ${formatCost(stats.total_cost_usd)} spent · last ${stats.since_days} days`
            : "loading…"}
        </span>
        <div className="ml-auto flex items-center gap-1 text-micro">
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
            onClick={() => void statsQuery.refetch()}
            disabled={loading}
          >
            {loading ? "…" : "Refresh"}
          </Button>
        </div>
      </header>

      <p className="text-micro text-fg-muted">
        Three views across your fleet: spend trend per day stacked by
        workflow, per-workflow reliability with fail rate and duration
        percentiles, and tail latency (P95) to spot bots that crawl.
      </p>

      {error && (
        <div className="text-danger-fg text-micro" role="alert">
          {error}
        </div>
      )}

      {!stats && loading && (
        <div className="space-y-4">
          <Skeleton className="h-56 w-full" />
          <Skeleton className="h-40 w-full" />
          <Skeleton className="h-40 w-full" />
        </div>
      )}

      {stats && stats.total_runs === 0 &&
        (cloud ? (
          <EmptyState
            title="No runs in this window"
            message={`No runs in the last ${stats.since_days} days. Pick a bot from the gallery to populate this dashboard.`}
            action={
              <Link href="/bots">
                <Button variant="primary" size="sm">
                  Browse bots
                </Button>
              </Link>
            }
          />
        ) : (
          <EmptyState
            title="No runs in this window"
            message={`No runs in the last ${stats.since_days} days. Launch one from /whats-next or the editor to populate this dashboard.`}
            action={
              <Link href="/whats-next">
                <Button variant="primary" size="sm">
                  Open /whats-next
                </Button>
              </Link>
            }
          />
        ))}

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
                color: workflowColors[w.workflow] ?? "var(--color-fg-subtle)",
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
    <section className="border border-border-default rounded-[var(--radius-lg)] bg-surface-1 shadow-[var(--shadow-sm)] p-4 space-y-2">
      <header className="flex items-baseline gap-2">
        <h2 className="text-title font-semibold text-fg-default">{title}</h2>
        {subtitle && (
          <span className="text-micro text-fg-muted">— {subtitle}</span>
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
      <p className="text-micro text-fg-muted italic">No cost recorded in this window.</p>
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
              className="fill-fg-muted text-caption"
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
              fill={colors[wf] ?? "var(--color-fg-subtle)"}
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
            className="fill-fg-muted text-caption"
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
      <p className="text-micro text-fg-muted italic">
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
          <div key={w.workflow} className="text-micro">
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
                  background: colors[w.workflow] ?? "var(--color-fg-subtle)",
                  opacity: 0.45,
                }}
              />
              <div
                className="absolute inset-y-0 left-0"
                style={{
                  width: `${p50Pct}%`,
                  background: colors[w.workflow] ?? "var(--color-fg-subtle)",
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
    <Table
      caption="Per-workflow run statistics: run count, status histogram, fail rate, P50/P95 duration, total cost"
      density="sm"
    >
      <THead>
        <Th>Workflow</Th>
        <Th align="right">Runs</Th>
        <Th>Status</Th>
        <Th align="right">Fail rate</Th>
        <Th align="right">P50 / P95</Th>
        <Th align="right">Cost</Th>
      </THead>
      <TBody>
        {workflows.map((w) => {
          const rateClass =
            w.fail_rate >= 0.5
              ? "text-danger-fg"
              : w.fail_rate >= 0.2
                ? "text-warning-fg"
                : "text-fg-default";
          return (
            <Tr key={w.workflow}>
              <Td>
                <span
                  className="inline-block w-2 h-2 rounded-full mr-2 align-middle"
                  style={{ background: colors[w.workflow] ?? "var(--color-fg-subtle)" }}
                />
                <span className="font-mono text-fg-default">{w.workflow}</span>
              </Td>
              <Td align="right" className="tabular-nums text-fg-default">
                {w.run_count}
                <span className="text-fg-subtle text-caption ml-1">
                  ({totalRuns > 0 ? Math.round((w.run_count / totalRuns) * 100) : 0}%)
                </span>
              </Td>
              <Td>
                <StatusHistogram counts={w.counts_by_status} total={w.run_count} />
              </Td>
              <Td align="right" className={`tabular-nums ${rateClass}`}>
                {(w.fail_rate * 100).toFixed(0)}%
                <span className="text-fg-subtle text-caption ml-1">
                  ({w.fail_count}/{w.run_count})
                </span>
              </Td>
              <Td align="right" className="tabular-nums text-fg-muted">
                {formatMs(w.duration_p50_sec * 1000)} /{" "}
                <span className="text-fg-default">
                  {formatMs(w.duration_p95_sec * 1000)}
                </span>
              </Td>
              <Td align="right" className="tabular-nums text-fg-default">
                {formatCost(w.total_cost_usd)}
              </Td>
            </Tr>
          );
        })}
      </TBody>
    </Table>
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

// Maps each run status to a semantic design-system token (resolved at
// paint time against the active theme, so the charts invert for light
// mode for free). queued + cancelled intentionally collapse onto the
// muted foreground — both are inert, non-running states.
function statusColor(status: string): string {
  switch (status) {
    case "finished":
      return "var(--color-success)";
    case "running":
      return "var(--color-live)";
    case "paused_waiting_human":
      return "var(--color-iteration-1)"; // purple
    case "queued":
      return "var(--color-fg-subtle)";
    case "failed_resumable":
      return "var(--color-warning)";
    case "failed":
      return "var(--color-danger)";
    case "cancelled":
      return "var(--color-fg-subtle)";
    default:
      return "var(--color-fg-subtle)";
  }
}

// ── Legend ──────────────────────────────────────────────────────────────────

function Legend({
  entries,
}: {
  entries: { label: string; color: string; value: string }[];
}) {
  return (
    <div className="flex flex-wrap gap-3 text-micro">
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
// The design-system iteration palette: six intentionally-distinct hues
// (theme-aware via app.css). A workflow keeps a stable hue across the
// chart, table, and legend; >6 distinct workflows wrap the cycle.
const PALETTE = [
  "var(--color-iteration-0)", // cyan
  "var(--color-iteration-1)", // purple
  "var(--color-iteration-2)", // amber
  "var(--color-iteration-3)", // teal
  "var(--color-iteration-4)", // pink
  "var(--color-iteration-5)", // lime
];

function assignWorkflowColors(workflows: string[]): Record<string, string> {
  const out: Record<string, string> = {};
  workflows.forEach((wf, i) => {
    out[wf] = PALETTE[i % PALETTE.length] ?? "var(--color-fg-subtle)";
  });
  return out;
}
