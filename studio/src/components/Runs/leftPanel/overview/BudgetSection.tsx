import { useState } from "react";

import { Meter } from "@/components/ui";
import { useRunBudgetCaps } from "@/hooks/useRunBudgetCaps";
import { useRunCheckpointBudget } from "@/hooks/useRunCheckpointBudget";
import type { RunMetrics } from "@/hooks/useRunMetrics";
import { formatCost, formatMs, formatTokens } from "@/lib/format";
import type { RunHeader } from "@/api/runs";
import { isRunSteerable, RaiseBudgetDialog } from "../../runSteering";

import { Section } from "../InfoPrimitives";

interface BudgetSectionProps {
  run: RunHeader;
  metrics: RunMetrics;
}

// BudgetSection meters the run against its budget: cost / tokens /
// iterations / duration, each drawn against the effective cap the run
// launched with. Consumption prefers the persisted checkpoint totals
// (authoritative across resume segments) and falls back to the live
// event-derived metrics. Rendered only when the run actually declared a
// budget — without caps, the header vitals already carry raw cost/tokens
// and a bare-stat repeat here would just be noise.
export function BudgetSection({ run, metrics }: BudgetSectionProps) {
  const caps = useRunBudgetCaps(run);
  const cp = useRunCheckpointBudget(run);
  const [raiseOpen, setRaiseOpen] = useState(false);

  if (!caps.hasAny) return null;

  const steerable = isRunSteerable(run.status);

  const cost = cp?.costUsd ?? metrics.costUsd;
  const tokens = cp?.tokensUsed ?? metrics.totalTokens;
  const iters = cp?.iterationsUsed ?? 0;
  const durationMs = metrics.durationMs;
  const intFmt = (v: number) => String(Math.round(v));

  const rows = [
    {
      key: "cost",
      show: caps.maxCostUsd != null || cost > 0,
      node: (
        <Meter
          label="Cost"
          value={cost}
          max={caps.maxCostUsd ?? undefined}
          formatValue={formatCost}
        />
      ),
    },
    {
      key: "tokens",
      show: caps.maxTokens != null || tokens > 0,
      node: (
        <Meter
          label="Tokens"
          value={tokens}
          max={caps.maxTokens ?? undefined}
          formatValue={formatTokens}
        />
      ),
    },
    {
      key: "iterations",
      show: caps.maxIterations != null || iters > 0,
      node: (
        <Meter
          label="Iterations"
          value={iters}
          max={caps.maxIterations ?? undefined}
          formatValue={intFmt}
        />
      ),
    },
    {
      key: "duration",
      show: caps.maxDurationMs != null || durationMs > 0,
      node: (
        <Meter
          label="Duration"
          value={durationMs}
          max={caps.maxDurationMs ?? undefined}
          formatValue={formatMs}
        />
      ),
    },
  ].filter((r) => r.show);

  return (
    <Section
      title="Budget"
      headerRight={
        steerable ? (
          <button
            type="button"
            className="text-xs text-accent hover:underline"
            onClick={() => setRaiseOpen(true)}
            title="Raise the run's budget caps live (raise-only; survives resume)"
          >
            Raise caps…
          </button>
        ) : undefined
      }
    >
      <div className="space-y-2 pt-0.5">
        {rows.map((r) => (
          <div key={r.key}>{r.node}</div>
        ))}
      </div>
      {steerable && (
        <RaiseBudgetDialog
          open={raiseOpen}
          onOpenChange={setRaiseOpen}
          runId={run.id}
          current={{
            maxCostUsd: caps.maxCostUsd,
            maxTokens: caps.maxTokens,
            maxIterations: caps.maxIterations,
            maxDuration: run.budget?.max_duration ?? null,
          }}
        />
      )}
    </Section>
  );
}
