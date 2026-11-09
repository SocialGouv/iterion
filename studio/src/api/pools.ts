// Key pools — REST client for the configurable pools (ADR-164): named
// pools of provider credentials, their entries (pledges with an
// entry-level route binding), and the per-scope attachments that opt a
// cascade level into one pool. Mirrors pkg/server/pool_routes.go.

import { FeatureUnavailableError, guard404, request } from "./client";

export interface KeyPool {
  id: string;
  org_id?: string;
  name?: string;
  enabled: boolean;
  owner_type: string; // org | user | platform
  owner_id?: string;
  visibility: string; // audience | private
  audience: {
    teams?: string[];
    orgs?: string[];
    contributors?: boolean;
    all_teams?: boolean;
  };
  updated_at?: string;
}

export interface KeyPoolEntry {
  id: string;
  pool_id: string;
  lender: string;
  source: string; // oauth | api_key
  ref: string;
  key_id?: string;
  backend?: string;
  model?: string;
  priority?: number;
  enabled: boolean;
  limits: { max_usd_per_day?: number; max_usd_per_week?: number; max_runs_per_day?: number; max_concurrent_runs?: number };
  status: string; // active | paused | cooling | near_limit | exhausted | serving | unhealthy | out_of_hours | bot_filtered
  health: string;
  health_detail?: string;
  cooldown_until?: string;
}

export interface PoolStrategy {
  mode?: string; // priority | least_consumed
  margin_pct?: number;
  triggers?: string[];
  reset_hold?: boolean;
}

export interface PoolAttachment {
  scope_id: string;
  pool_id: string;
  enabled: boolean;
  strategy: PoolStrategy;
  limits: { max_runs_per_day?: number; max_usd_per_day?: number };
  updated_at?: string;
}

export interface PoolAttachmentPut {
  pool_id: string;
  enabled?: boolean;
  mode?: string;
  margin_pct?: number;
  triggers?: string[];
  reset_hold?: boolean;
  max_runs_per_day?: number;
  max_usd_per_day?: number;
  expected_updated_at?: string;
}

export interface PoolEntryPut {
  source: string;
  ref: string;
  key_id?: string;
  enabled?: boolean;
  backend?: string;
  model?: string;
  priority?: number;
  lend_scope?: string;
}

// The list calls resolve to [] on servers without the feature (404) so
// the surfaces stay empty instead of erroring on an older backend.
export async function listMyPools(): Promise<KeyPool[]> {
  try {
    return await guard404("key_pools", () => request<KeyPool[]>("/me/pools"));
  } catch (err) {
    if (err instanceof FeatureUnavailableError) return [];
    throw err;
  }
}

export async function listOrgPools(orgID: string): Promise<KeyPool[]> {
  try {
    return await guard404("key_pools", () => request<KeyPool[]>(`/orgs/${orgID}/pools`));
  } catch (err) {
    if (err instanceof FeatureUnavailableError) return [];
    throw err;
  }
}

export async function listPlatformPools(): Promise<KeyPool[]> {
  try {
    return await guard404("key_pools", () => request<KeyPool[]>("/admin/llm/pools"));
  } catch (err) {
    if (err instanceof FeatureUnavailableError) return [];
    throw err;
  }
}

export async function createPool(body: {
  name?: string;
  visibility?: string;
  enabled?: boolean;
}): Promise<KeyPool> {
  return request("/me/pools", { method: "POST", body: JSON.stringify(body) });
}

export async function createOrgPool(orgID: string, body: { name?: string }): Promise<KeyPool> {
  return request(`/orgs/${orgID}/pools`, { method: "POST", body: JSON.stringify(body) });
}

export async function createPlatformPool(body: { name?: string }): Promise<KeyPool> {
  return request("/admin/llm/pools", { method: "POST", body: JSON.stringify(body) });
}

export async function listPoolEntries(poolID: string): Promise<KeyPoolEntry[]> {
  return request(`/pools/${poolID}/entries`);
}

export async function putPoolEntry(poolID: string, body: PoolEntryPut): Promise<KeyPoolEntry> {
  return request(`/pools/${poolID}/entries`, { method: "PUT", body: JSON.stringify(body) });
}

export async function deletePoolEntry(poolID: string, entryID: string): Promise<void> {
  await request(`/pools/${poolID}/entries/${entryID}`, { method: "DELETE" });
}

export async function importPoolEntry(
  poolID: string,
  body: { source: string; credentials_json: string; model?: string; priority?: number },
): Promise<KeyPoolEntry> {
  return request(`/pools/${poolID}/entries/import`, { method: "POST", body: JSON.stringify(body) });
}

export async function resetPoolEntry(poolID: string, entryID: string): Promise<KeyPoolEntry> {
  return request(`/pools/${poolID}/entries/${entryID}/reset`, { method: "POST", body: JSON.stringify({}) });
}

// The attachment reads resolve to null on servers without the feature
// (404): "no attachment at this level" and "older backend" both mean the
// editor has nothing to show yet — the section stays hidden.
export async function getOrgPoolAttachment(orgID: string): Promise<PoolAttachment | null> {
  try {
    return await guard404("pool_attachment", () => request<PoolAttachment>(`/orgs/${orgID}/pool-attachment`));
  } catch (err) {
    if (err instanceof FeatureUnavailableError) return null;
    throw err;
  }
}

export async function putOrgPoolAttachment(orgID: string, body: PoolAttachmentPut): Promise<PoolAttachment> {
  return request(`/orgs/${orgID}/pool-attachment`, { method: "PUT", body: JSON.stringify(body) });
}

export async function deleteOrgPoolAttachment(orgID: string, expectedUpdatedAt?: string): Promise<void> {
  await request(`/orgs/${orgID}/pool-attachment`, {
    method: "DELETE",
    body: JSON.stringify({ expected_updated_at: expectedUpdatedAt }),
  });
}

export async function getTeamPoolAttachment(teamID: string): Promise<PoolAttachment | null> {
  try {
    return await guard404("pool_attachment", () => request<PoolAttachment>(`/teams/${teamID}/pool-attachment`));
  } catch (err) {
    if (err instanceof FeatureUnavailableError) return null;
    throw err;
  }
}

export async function putTeamPoolAttachment(teamID: string, body: PoolAttachmentPut): Promise<PoolAttachment> {
  return request(`/teams/${teamID}/pool-attachment`, { method: "PUT", body: JSON.stringify(body) });
}

export async function deleteTeamPoolAttachment(teamID: string, expectedUpdatedAt?: string): Promise<void> {
  await request(`/teams/${teamID}/pool-attachment`, {
    method: "DELETE",
    body: JSON.stringify({ expected_updated_at: expectedUpdatedAt }),
  });
}

export async function syncPools(): Promise<{ status?: string; verdict?: string }> {
  return request("/admin/llm/pools/sync", { method: "POST", body: JSON.stringify({}) });
}
