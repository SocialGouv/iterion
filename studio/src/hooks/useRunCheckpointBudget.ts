import { useMemo } from "react";

import type { RunHeader } from "@/api/runs";

export interface RunCheckpointBudget {
  tokensUsed: number;
  costUsd: number;
  iterationsUsed: number;
  elapsedMs: number;
}

// useRunCheckpointBudget reads the persisted budget consumption off the
// run checkpoint. This is authoritative across resume segments — unlike
// the live event-derived totals (useRunMetrics), which restart from zero
// each resume. Returns null when the checkpoint carries none of the
// budget_* fields (a fresh run, or one that hasn't checkpointed a budget
// yet), so callers fall back to the live totals.
export function useRunCheckpointBudget(
  run: RunHeader | null,
): RunCheckpointBudget | null {
  return useMemo(() => {
    const c = run?.checkpoint;
    if (!c) return null;
    const tokensUsed = c.budget_tokens_used ?? 0;
    const costUsd = c.budget_cost_usd ?? c.cost_usd_total ?? 0;
    const iterationsUsed = c.budget_iterations_used ?? 0;
    const elapsedMs =
      typeof c.budget_elapsed_ns === "number" ? c.budget_elapsed_ns / 1e6 : 0;
    if (
      tokensUsed === 0 &&
      costUsd === 0 &&
      iterationsUsed === 0 &&
      elapsedMs === 0
    ) {
      return null;
    }
    return { tokensUsed, costUsd, iterationsUsed, elapsedMs };
  }, [run?.checkpoint]);
}
