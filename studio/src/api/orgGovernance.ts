// Org governance — REST client for the org-admin-managed controls:
// the ex-ante provisioning approval queue (Org.RequireProvisionApproval),
// the org settings flag itself, and the per-team executor caps delegated
// to org admins. Mirrors pkg/server/forge_approval_routes.go +
// pkg/server/orgs_routes.go.

import { FeatureUnavailableError, guard404, request } from "./client";
import type { ForgeProvisionResult } from "./forgeConnections";

// Mirrors forge.ProvisionApproval (pkg/forge/provision_approval_store.go).
export interface ProvisionApproval {
  id: string;
  org_id: string;
  tenant_id: string;
  connection_id: string;
  repo_full_name: string;
  bot_ids: string[];
  integration_id?: string;
  /** Replace semantics: on approval the requested bots become the EXACT
   *  bot set of the (existing) integration — the bots the set held that
   *  bot_ids drops are disabled (webhook + schedules torn down). */
  replace?: boolean;
  /** Snapshots the integration's live bot set at park time (update
   *  requests only): with replace, the names it holds that bot_ids drops
   *  are what the approval disables. Hand-typed: this route carries no
   *  openapi schema. */
  base_bot_ids?: string[];
  /** Snapshots of the integration's two operator sets at park time (the
   *  base_bot_ids precedent): a PARTIAL hold-label lift or an allowlist
   *  widening parks precisely because it re-arms automation, and the card
   *  names what the change lifts or widens against these. Absent means no
   *  delta is possible (new repo, or a stored set nothing can be lifted
   *  from). Hand-typed: this route carries no openapi schema. */
  base_hold_labels?: string[];
  base_label_allowlist?: string[];
  schedule_crons?: Record<string, string>;
  /** The spoken launch_vars map — null = the request was silent, an
   *  empty-but-present {} IS the whole-map clear (a legacy record parked
   *  before the Set flags shipped carries the empty value and no fact). */
  launch_vars?: Record<string, string>;
  /** The spoken launch_vars map REPLACES the stored one wholesale instead
   *  of merging — an empty map is a clear of every pin. */
  launch_vars_replace?: boolean;
  overlap?: string;
  auto_fix?: boolean;
  /** null = the request was silent about the pause; [] = the lift the
   *  approval executes. */
  hold_labels?: string[];
  /** null = the request was silent about dispatch; [] = the widening. */
  label_allowlist?: string[];
  /** The three Set facts mark that the request body NAMED launch_vars /
   *  hold_labels / label_allowlist: the fields' own empty values collapse
   *  to absent keys on this wire, so a spoken-empty gesture (the map
   *  clear, the pause lift, the dispatch widen) is only visible through
   *  them. Absent = the request was silent — the stored value stays. */
  launch_vars_set?: boolean;
  hold_labels_set?: boolean;
  label_allowlist_set?: boolean;
  /** The routing block the request speaks about (pkg/llmroute, ADR-129) —
   *  the payload this gate exists to arbitrate. routing_set marks that the
   *  request SPOKE: a policy present REPLACES the stored block, one absent
   *  CLEARS it. Hand-typed: this route carries no openapi schema. */
  routing?: Record<string, unknown> | null;
  routing_set?: boolean;
  requested_by: string;
  created_at: string;
}

export interface OrgSettings {
  require_provision_approval: boolean;
}

// The two list calls resolve to [] on servers without the feature (404)
// so the surfaces stay empty instead of erroring on an older backend.
export async function listOrgProvisionApprovals(orgID: string): Promise<ProvisionApproval[]> {
  try {
    const r = await guard404("provision_approvals", () =>
      request<{ approvals: ProvisionApproval[] }>(`/orgs/${orgID}/provision-approvals`),
    );
    return r.approvals ?? [];
  } catch (err) {
    if (err instanceof FeatureUnavailableError) return [];
    throw err;
  }
}

export async function listTeamProvisionApprovals(teamID: string): Promise<ProvisionApproval[]> {
  try {
    const r = await guard404("provision_approvals", () =>
      request<{ approvals: ProvisionApproval[] }>(`/teams/${teamID}/provision-approvals`),
    );
    return r.approvals ?? [];
  } catch (err) {
    if (err instanceof FeatureUnavailableError) return [];
    throw err;
  }
}

export async function approveProvision(
  orgID: string,
  approvalID: string,
): Promise<ForgeProvisionResult> {
  return request(`/orgs/${orgID}/provision-approvals/${approvalID}/approve`, {
    method: "POST",
  });
}

export async function rejectProvision(
  orgID: string,
  approvalID: string,
  reason?: string,
): Promise<void> {
  await request<void>(`/orgs/${orgID}/provision-approvals/${approvalID}/reject`, {
    method: "POST",
    body: JSON.stringify({ reason: reason ?? "" }),
  });
}

export async function getOrgSettings(orgID: string): Promise<OrgSettings> {
  return request(`/orgs/${orgID}/settings`);
}

export async function updateOrgSettings(
  orgID: string,
  patch: Partial<OrgSettings>,
): Promise<OrgSettings> {
  return request(`/orgs/${orgID}/settings`, {
    method: "PATCH",
    body: JSON.stringify(patch),
  });
}

// Slim view of GET /api/orgs/{id}/teams (teamSummaryView) — carries the
// per-team caps the governance tab edits.
export interface OrgTeamSummary {
  id: string;
  name: string;
  slug: string;
  status: string;
  personal?: boolean;
  max_concurrent_runs?: number;
  launch_rate_per_min?: number;
}

export async function listOrgTeamSummaries(orgID: string): Promise<OrgTeamSummary[]> {
  const r = await request<{ teams: OrgTeamSummary[] }>(`/orgs/${orgID}/teams`);
  return r.teams ?? [];
}

export async function updateOrgTeamCaps(
  orgID: string,
  teamID: string,
  caps: { max_concurrent_runs?: number; launch_rate_per_min?: number },
): Promise<void> {
  await request(`/orgs/${orgID}/teams/${teamID}/caps`, {
    method: "PATCH",
    body: JSON.stringify(caps),
  });
}

// ---- Tenant routing policies (ADR-121 delivery 2) ----

// Mirrors llmroute.Policy (pkg/llmroute/policy.go) — the fields delivery 2
// folds per level. Served strict: unknown fields 400.
export interface RoutingPolicy {
  pair_order?: string[];
  triggers?: string[];
  refused_pinned_key?: string;
  strict?: boolean;
  locks?: string[];
}

// Mirrors server.routingPolicyView (pkg/server/routing_policy_routes.go).
export interface RoutingPolicyView {
  scope: string;
  policy: RoutingPolicy | null;
  origin: string;
  updated_at?: string;
  updated_by?: string;
}

export interface RoutingPolicyPut {
  routing: RoutingPolicy | null;
  expected_updated_at?: string;
}

export async function getOrgRoutingPolicy(orgID: string): Promise<RoutingPolicyView | null> {
  try {
    return await guard404("routing_policy", () => request<RoutingPolicyView>(`/orgs/${orgID}/routing-policy`));
  } catch (err) {
    if (err instanceof FeatureUnavailableError) return null;
    throw err;
  }
}

export async function putOrgRoutingPolicy(orgID: string, body: RoutingPolicyPut): Promise<RoutingPolicyView> {
  return request(`/orgs/${orgID}/routing-policy`, { method: "PUT", body: JSON.stringify(body) });
}

export async function getTeamRoutingPolicy(teamID: string): Promise<RoutingPolicyView | null> {
  try {
    return await guard404("routing_policy", () => request<RoutingPolicyView>(`/teams/${teamID}/routing-policy`));
  } catch (err) {
    if (err instanceof FeatureUnavailableError) return null;
    throw err;
  }
}

export async function putTeamRoutingPolicy(teamID: string, body: RoutingPolicyPut): Promise<RoutingPolicyView> {
  return request(`/teams/${teamID}/routing-policy`, { method: "PUT", body: JSON.stringify(body) });
}
