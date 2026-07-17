import { apiRequest } from "./client";

// Live-run steering: grant loop iterations / raise budget caps on a
// RUNNING run. The server answers with the truthful contract — 400
// unknown loop, 409 terminal/not-held/no-budget, 202 queued-but-busy,
// 200 applied-or-noop — surfaced here as ApiError for non-2xx.

export interface BumpLoopResponse {
  run_id: string;
  loop: string;
  delta?: number;
  extra?: number;
  effective_max?: number;
  current_iteration: number;
  noop?: boolean;
  noop_reason?: string;
  warning?: string;
}

export interface RaiseBudgetResponse {
  run_id: string;
  applied?: Record<string, unknown>;
  effective?: Record<string, unknown>;
  noop?: boolean;
  noop_reason?: string;
  warning?: string;
}

export interface RaiseBudgetCaps {
  max_cost_usd?: number;
  max_tokens?: number;
  max_iterations?: number;
  max_duration?: string;
}

export async function bumpLoop(
  runId: string,
  loopName: string,
  delta: number,
): Promise<BumpLoopResponse> {
  return apiRequest<BumpLoopResponse>(
    `/api/runs/${encodeURIComponent(runId)}/bump-loop`,
    {
      method: "POST",
      body: JSON.stringify({ loop_name: loopName, delta }),
    },
  );
}

export async function raiseBudget(
  runId: string,
  caps: RaiseBudgetCaps,
): Promise<RaiseBudgetResponse> {
  return apiRequest<RaiseBudgetResponse>(
    `/api/runs/${encodeURIComponent(runId)}/raise-budget`,
    {
      method: "POST",
      body: JSON.stringify({ budget: caps }),
    },
  );
}
