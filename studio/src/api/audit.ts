// Audit-log REST client. Mirrors pkg/server/audit_helper.go + pkg/audit.

import { FeatureUnavailableError, guard404, request } from "./client";

export { FeatureUnavailableError };

export interface AuditEvent {
  id: string;
  scope: "tenant" | "platform";
  tenant_id?: string;
  actor_id?: string;
  actor_kind?: "user" | "super_admin" | "webhook" | "system";
  action: string;
  target?: string;
  target_id?: string;
  meta?: Record<string, unknown>;
  ip?: string;
  user_agent?: string;
  created_at: string;
}

export interface AuditListResponse {
  events: AuditEvent[];
  next_offset: number;
}

export interface AuditQuery {
  action?: string;
  actor?: string;
  from?: string; // RFC3339
  to?: string;
  offset?: number;
  limit?: number;
}

function qstr(q: AuditQuery): string {
  const sp = new URLSearchParams();
  if (q.action) sp.set("action", q.action);
  if (q.actor) sp.set("actor", q.actor);
  if (q.from) sp.set("from", q.from);
  if (q.to) sp.set("to", q.to);
  if (q.offset && q.offset > 0) sp.set("offset", String(q.offset));
  if (q.limit && q.limit > 0) sp.set("limit", String(q.limit));
  const s = sp.toString();
  return s ? `?${s}` : "";
}

export function listTeamAudit(teamID: string, q: AuditQuery = {}): Promise<AuditListResponse> {
  return guard404("audit", () =>
    request<AuditListResponse>(`/teams/${encodeURIComponent(teamID)}/audit${qstr(q)}`),
  );
}

// listOrgAudit returns the org-scoped control-plane audit (members, SSO,
// settings). Org-admin gated server-side.
export function listOrgAudit(orgID: string, q: AuditQuery = {}): Promise<AuditListResponse> {
  return guard404("audit", () =>
    request<AuditListResponse>(`/orgs/${encodeURIComponent(orgID)}/audit${qstr(q)}`),
  );
}

export function listAdminAudit(q: AuditQuery = {}): Promise<AuditListResponse> {
  return guard404("audit", () => request<AuditListResponse>(`/admin/audit${qstr(q)}`));
}
