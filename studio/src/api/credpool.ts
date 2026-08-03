// Credential-pool API client: lending an LLM subscription to the shared
// pool, and the operator view of who is lending.
//
// The credential itself never appears here — it stays sealed server-side
// and is only ever unsealed into a run's bundle. What travels is the
// donor's TERMS and what has been drawn against them.

import { request as send } from "./client";
import type { OAuthKind } from "./byok";

/** Every ceiling is optional; 0 means "no limit on this axis". */
export interface PoolLimits {
  max_usd_per_day?: number;
  max_usd_per_week?: number;
  max_runs_per_day?: number;
  max_concurrent_runs?: number;
}

/** Hours/days the contribution is shareable in. Absent = always. */
export interface PoolWindow {
  timezone?: string;
  /** Inclusive start, exclusive end, local time. Equal = all day; start > end wraps midnight. */
  start_hour: number;
  end_hour: number;
  /** time.Weekday values (Sunday = 0). Empty = every day. */
  weekdays?: number[];
}

export interface PoolUsage {
  runs: number;
  cost_usd: number;
  input_tokens: number;
  output_tokens: number;
}

export type PledgeStatus =
  | "active"
  | "paused"
  | "cooling"
  | "out_of_hours"
  | "exhausted"
  | "unhealthy"
  | "bot_filtered";

export interface PledgeView {
  kind: OAuthKind;
  /** False when the subscription behind this pledge is no longer connected. */
  connected: boolean;
  enabled: boolean;
  status: PledgeStatus;
  limits: PoolLimits;
  window?: PoolWindow;
  bots?: string[];
  health: string;
  health_detail?: string;
  cooldown_until?: string;
  last_served_at?: string;
  /** ESTIMATED spend — a subscription bills nothing per call, so this is derived from tokens. */
  today: PoolUsage;
  this_week: PoolUsage;
  updated_at?: string;
}

export interface PledgeLease {
  run_id: string;
  bot_id?: string;
  tenant_id?: string;
  requester_id?: string;
  cost_usd: number;
  outcome?: string;
  closed: boolean;
  acquired_at: string;
}

export interface PledgeInput {
  enabled: boolean;
  limits: PoolLimits;
  window?: PoolWindow | null;
  bots?: string[];
}

export async function listMyPledges(): Promise<{ pledges: PledgeView[]; pool_id?: string }> {
  const res = await send<{ pledges: PledgeView[]; pool_id?: string }>("/me/pool");
  return { pledges: res.pledges ?? [], pool_id: res.pool_id };
}

export async function savePledge(kind: OAuthKind, input: PledgeInput): Promise<PledgeView> {
  return send(`/me/pool/${encodeURIComponent(kind)}`, {
    method: "PUT",
    body: JSON.stringify(input),
  });
}

export async function withdrawPledge(kind: OAuthKind): Promise<void> {
  await send(`/me/pool/${encodeURIComponent(kind)}`, { method: "DELETE" });
}

export async function listMyPoolHistory(): Promise<PledgeLease[]> {
  const res = await send<{ leases: PledgeLease[] }>("/me/pool/history");
  return res.leases ?? [];
}

// ---- Operator surface ----

/** Who may draw on the pool. A union of independent predicates. */
export interface PoolAudience {
  teams?: string[];
  orgs?: string[];
  /** Reciprocity: any active donor may draw, wherever they launch from. */
  contributors?: boolean;
  all_teams?: boolean;
}

export interface PoolDonor {
  user_id: string;
  kind: OAuthKind;
  status: PledgeStatus;
  health: string;
  cooldown_until?: string;
  last_served_at?: string;
  today_runs: number;
  today_cost_usd: number;
}

export interface PoolView {
  id: string;
  org_id: string;
  name?: string;
  enabled: boolean;
  audience: PoolAudience;
  donors: PoolDonor[];
}

export async function getTeamPool(teamID: string): Promise<PoolView> {
  return send(`/teams/${encodeURIComponent(teamID)}/pool`);
}

export async function saveTeamPool(
  teamID: string,
  input: { name?: string; enabled?: boolean; audience?: PoolAudience },
): Promise<PoolView> {
  return send(`/teams/${encodeURIComponent(teamID)}/pool`, {
    method: "PUT",
    body: JSON.stringify(input),
  });
}
