// Cross-run stats client — backs the Runs analytics dashboard
// (reached from the Runs list toolbar; route still served at
// /insights to preserve operator bookmarks).
//
// Mirrors pkg/server/runs_stats.go's StatsResponse shape. The
// backend walks events.jsonl per run to sum per-node `_cost_usd`,
// so a Refresh costs ~sub-second on hundreds of runs; we don't poll
// automatically (the dashboard is a manual surface).

import { apiRequest } from "./client";

export interface CostBucket {
  day: string; // "YYYY-MM-DD"
  cost_by_workflow: Record<string, number>;
  total: number;
}

export interface WorkflowStats {
  workflow: string;
  run_count: number;
  fail_count: number;
  fail_rate: number; // 0..1
  duration_p50_sec: number;
  duration_p95_sec: number;
  total_cost_usd: number;
  counts_by_status: Record<string, number>;
}

export interface StatsResponse {
  since_days: number;
  total_runs: number;
  total_cost_usd: number;
  cost_by_day: CostBucket[];
  workflows: WorkflowStats[];
}

export function getRunsStats(sinceDays = 30): Promise<StatsResponse> {
  const q = new URLSearchParams({ since_days: String(sinceDays) }).toString();
  return apiRequest<StatsResponse>(`/api/v1/runs/stats?${q}`);
}
