// Super-admin org (team) console — REST client.
// Mirrors pkg/server/admin_orgs_routes.go. "org" is the public alias
// for the internal Team/tenant.

import { guard404, request } from "./client";
import { apiGet, apiPatch, apiPost } from "./typed";

export interface OrgView {
  id: string;
  name: string;
  slug: string;
  status: string; // active | suspended | read_only
  personal?: boolean;
  // Org-level monthly budget (0/omitted = platform default). Concurrency
  // and launch-rate caps are team-level (managed per team), not here.
  monthly_run_quota?: number;
  memory_quota_bytes?: number;
  monthly_cost_cap_usd?: number;
  suspend_reason?: string;
  created_at?: string;
  // Set when status === "pending_deletion": when the nightly sweeper may purge.
  purge_after?: string;
}

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
  return (res.orgs ?? []) as OrgView[];
}

export async function createOrg(input: {
  name: string;
  slug?: string;
  owner_email?: string;
}): Promise<OrgView> {
  // Body is type-checked against the spec's createOrgReq.
  return (await apiPost("/api/admin/orgs", { body: input })) as OrgView;
}

export async function getOrgUsage(id: string): Promise<OrgUsage> {
  return request<OrgUsage>(`/admin/orgs/${encodeURIComponent(id)}/usage`);
}

// Soft-delete an org (super-admin): marks it pending_deletion with a 24h grace,
// after which a nightly sweeper hard-purges the org + teams + ALL their data.
// Blocked immediately; refuses the caller's active org (409). Returns the org
// in its new pending state.
export async function deleteOrg(id: string): Promise<OrgView> {
  return request<OrgView>(`/admin/orgs/${encodeURIComponent(id)}`, { method: "DELETE" });
}

// Cancel a pending org deletion within the grace window (super-admin).
export async function restoreOrg(id: string): Promise<OrgView> {
  return request<OrgView>(`/admin/orgs/${encodeURIComponent(id)}/restore`, { method: "POST" });
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
  return (await apiPatch("/api/admin/orgs/{id}", { params: { id }, body: patch })) as OrgView;
}

export async function setOrgStatus(id: string, status: string, reason?: string): Promise<OrgView> {
  return request<OrgView>(`/admin/orgs/${encodeURIComponent(id)}/status`, {
    method: "POST",
    body: JSON.stringify({ status, reason }),
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
