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
  replace?: boolean;
  launch_vars?: Record<string, string>;
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
