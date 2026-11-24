// Cross-org runs — super-admin, #2148. One listing of every org's runs,
// filterable by org/team and state, server-side paginated across orgs by a
// keyset cursor. Read-only by design: opening ONE run rides the ordinary
// /api/runs/{id} choke point, not this client. Row shapes come from the
// generated OpenAPI types (schema.ts) so the client cannot drift from the
// server's view; each row names its team and org and carries the run's
// sovereign runner-pool stamp (#2029) for the visual marking (A19).

import { request } from "./client";
import type { components } from "./schema";
import type { OrgView } from "./orgs";

export type AdminRunsResponse = components["schemas"]["adminRunsResponse"];
export type AdminRunRow = components["schemas"]["adminRunRow"];

export interface AdminRunsQuery {
  orgId?: string;
  teamId?: string;
  status?: string;
  cursor?: string;
  limit?: number;
}

export function listAdminRuns(q: AdminRunsQuery = {}): Promise<AdminRunsResponse> {
  const sp = new URLSearchParams();
  if (q.orgId) sp.set("org_id", q.orgId);
  if (q.teamId) sp.set("team_id", q.teamId);
  if (q.status) sp.set("status", q.status);
  if (q.cursor) sp.set("cursor", q.cursor);
  if (q.limit) sp.set("limit", String(q.limit));
  const s = sp.toString();
  return request<AdminRunsResponse>(`/admin/runs${s ? `?${s}` : ""}`);
}

// The org picker's options — the same super-admin org list the console
// home renders.
export async function listAdminOrgs(): Promise<OrgView[]> {
  const out = await request<{ orgs: OrgView[] }>("/admin/orgs");
  return out.orgs;
}
