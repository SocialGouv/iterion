import { useMemo } from "react";

import type { RunHeader } from "@/api/runs";
import { parseGoDuration } from "@/lib/duration";

export interface RunBudgetCaps {
  maxCostUsd: number | null;
  maxTokens: number | null;
  maxIterations: number | null;
  maxDurationMs: number | null;
  /** True when at least one dimension carries a real ceiling. */
  hasAny: boolean;
}

// useRunBudgetCaps reads the run's effective budget caps off the wire
// (RunHeader.budget), parsing the Go duration string to ms. A dimension
// is null when the workflow declared no ceiling there — the runtime
// treats 0 as "no cap" and RunBudget omits zero fields — so a Meter fed
// a null max degrades to a bare stat instead of a misleading full gauge.
export function useRunBudgetCaps(run: RunHeader | null): RunBudgetCaps {
  return useMemo(() => {
    const b = run?.budget;
    const pos = (n: number | undefined): number | null =>
      typeof n === "number" && n > 0 ? n : null;
    const durMs = parseGoDuration(b?.max_duration);
    const maxCostUsd = pos(b?.max_cost_usd);
    const maxTokens = pos(b?.max_tokens);
    const maxIterations = pos(b?.max_iterations);
    const maxDurationMs = durMs != null && durMs > 0 ? durMs : null;
    return {
      maxCostUsd,
      maxTokens,
      maxIterations,
      maxDurationMs,
      hasAny:
        maxCostUsd != null ||
        maxTokens != null ||
        maxIterations != null ||
        maxDurationMs != null,
    };
  }, [run?.budget]);
}
