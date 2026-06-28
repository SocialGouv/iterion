// Org / team usage REST client. Mirrors pkg/server/admin_orgs_routes.go.
// Two endpoints, identical shape:
//   GET /api/teams/{id}/usage          — any member of the team
//   GET /api/admin/orgs/{id}/usage     — super-admin only

import { FeatureUnavailableError, guard404, request } from "./client";

export { FeatureUnavailableError };

export interface OrgUsageOrg {
  id: string;
  name: string;
  slug: string;
  status: string;
  personal?: boolean;
  monthly_run_quota?: number;
  memory_quota_bytes?: number;
  monthly_cost_cap_usd?: number;
  max_concurrent_runs?: number;
  launch_rate_per_min?: number;
  suspend_reason?: string;
  created_at?: string;
}

// OrgUsage mirrors pkg/server.orgUsageView. Counter-backed fields read
// 0 in local mode where the underlying store isn't wired.
export interface OrgUsage {
  org: OrgUsageOrg;
  members: number;
  teams?: number;
  effective_memory_quota_bytes: number;
  monthly_run_quota: number;

  runs_this_month: number;
  cost_usd_this_month: number;
  input_tokens_this_month: number;
  output_tokens_this_month: number;
  monthly_cost_cap_usd?: number;
  max_concurrent_runs?: number;

  active_runs: number;
  webhook_calls_this_month: number;
  memory_used_bytes: number;
  api_key_count: number;
  generic_secret_count: number;
  bot_binding_count: number;
  webhook_count: number;
}

// getOrgUsage is the org-admin self-serve consumption view (any member of
// the org). Distinct from getAdminOrgUsage, which is the super-admin path.
export function getOrgUsage(orgID: string): Promise<OrgUsage> {
  return guard404("usage", () => request<OrgUsage>(`/orgs/${encodeURIComponent(orgID)}/usage`));
}

export function getAdminOrgUsage(orgID: string): Promise<OrgUsage> {
  return guard404("usage", () => request<OrgUsage>(`/admin/orgs/${encodeURIComponent(orgID)}/usage`));
}

// ---- formatting helpers ----

const GiB = 1 << 30;
const MiB = 1 << 20;

export function fmtBytes(n: number): string {
  if (!n || n <= 0) return "0 B";
  if (n >= GiB) return `${(n / GiB).toFixed(n % GiB === 0 ? 0 : 1)} GiB`;
  if (n >= MiB) return `${(n / MiB).toFixed(n % MiB === 0 ? 0 : 1)} MiB`;
  if (n >= 1024) return `${(n / 1024).toFixed(0)} KiB`;
  return `${n} B`;
}

export function fmtUSD(n?: number): string {
  if (n == null || Number.isNaN(n)) return "—";
  if (n === 0) return "$0.00";
  if (n < 0.01) return "<$0.01";
  return `$${n.toFixed(2)}`;
}

export function pct(used: number, quota?: number): number | null {
  if (!quota || quota <= 0) return null;
  return Math.max(0, Math.min(100, Math.round((used / quota) * 100)));
}
