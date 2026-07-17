// Super-admin org (team) console — REST client.
// Mirrors pkg/server/admin_orgs_routes.go. "org" is the public alias
// for the internal Team/tenant.

import { guard404, request } from "./client";
import type { components } from "./schema";
import { apiDelete, apiGet, apiPatch, apiPost } from "./typed";

// OrgView IS the spec's orgView schema (generated from the Go orgView struct) —
// single source of truth, so the response types below need no cast and can't
// drift from the server. status is "active | suspended | read_only |
// pending_deletion"; purge_after is set only in the pending state.
export type OrgView = components["schemas"]["orgView"];

// Backwards-compatible: the full-shape usage view lives in api/usage.ts
// (OrgUsage there). Keeping the slim alias here so existing call-sites
// continue to type-check while widening to the new admin payload.
export interface OrgUsage {
  org: OrgView;
  members: number;
  effective_memory_quota_bytes: number;
  monthly_run_quota: number;
  // Optional counters exposed by the cloud (Mongo) store. Local mode
  // leaves them zero.
  runs_this_month?: number;
  cost_usd_this_month?: number;
  input_tokens_this_month?: number;
  output_tokens_this_month?: number;
  monthly_cost_cap_usd?: number;
  max_concurrent_runs?: number;
  active_runs?: number;
  webhook_calls_this_month?: number;
  memory_used_bytes?: number;
  api_key_count?: number;
  generic_secret_count?: number;
  bot_binding_count?: number;
  webhook_count?: number;
}

export async function listOrgs(): Promise<OrgView[]> {
  // guard404 → FeatureUnavailableError when /api/admin/orgs isn't registered
  // (local/desktop mode — orgs are a cloud-only concept), so the page renders
  // a "cloud-mode feature" notice instead of a raw "404 no such API endpoint".
  // Typed against the OpenAPI spec (apiGet narrows path + response from
  // schema.ts); guard404 still maps the local-mode 404 to FeatureUnavailable.
  const res = await guard404("admin", () => apiGet("/api/admin/orgs"));
  return res.orgs ?? [];
}

export async function createOrg(input: {
  name: string;
  slug?: string;
  owner_email?: string;
}): Promise<OrgView> {
  // Body is type-checked against the spec's createOrgReq.
  return apiPost("/api/admin/orgs", { body: input });
}

// createOrgTeam creates a team inside an org (org admin/owner or
// super-admin). Server: POST /api/teams with an explicit org_id override.
export async function createOrgTeam(input: {
  name: string;
  slug?: string;
  org_id: string;
}): Promise<{ id: string; name: string; slug: string; personal?: boolean }> {
  return await request(`/teams`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
}

export async function getOrgUsage(id: string): Promise<OrgUsage> {
  return (await apiGet("/api/admin/orgs/{id}/usage", { params: { id } })) as OrgUsage;
}

// Soft-delete an org (super-admin): marks it pending_deletion with a 24h grace,
// after which a nightly sweeper hard-purges the org + teams + ALL their data.
// Blocked immediately; refuses the caller's active org (409). Returns the org
// in its new pending state.
export async function deleteOrg(id: string): Promise<OrgView> {
  return apiDelete("/api/admin/orgs/{id}", { params: { id } });
}

// Cancel a pending org deletion within the grace window (super-admin).
export async function restoreOrg(id: string): Promise<OrgView> {
  return apiPost("/api/admin/orgs/{id}/restore", { params: { id } });
}

export async function updateOrg(
  id: string,
  patch: {
    name?: string;
    slug?: string;
    monthly_run_quota?: number;
    memory_quota_bytes?: number;
    monthly_cost_cap_usd?: number;
  },
): Promise<OrgView> {
  // Path-param + body type-checked against the spec (updateOrgReq).
  return apiPatch("/api/admin/orgs/{id}", { params: { id }, body: patch });
}

export async function setOrgStatus(id: string, status: string, reason?: string): Promise<OrgView> {
  // Body type-checked against the spec's setOrgStatusReq.
  return apiPost("/api/admin/orgs/{id}/status", {
    params: { id },
    body: { status, reason },
  });
}

const GiB = 1 << 30;

/** Render a byte quota as a compact GiB string. */
export function fmtQuotaGiB(bytes?: number): string {
  if (!bytes || bytes <= 0) return "default";
  return `${(bytes / GiB).toFixed(bytes % GiB === 0 ? 0 : 1)} GiB`;
}

/** Convert a GiB number to bytes for the API. */
export function gibToBytes(gib: number): number {
  return Math.round(gib * GiB);
}
