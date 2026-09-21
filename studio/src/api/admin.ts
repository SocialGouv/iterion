// Super-admin user console — REST client. Mirrors auth_routes.go's
// /api/admin/users handlers.

import { FeatureUnavailableError, guard404, request } from "./client";
import type { OrgRole, Role, UserStatus, UserView } from "./auth";

export { FeatureUnavailableError };

export interface AdminUsersResponse {
  users: UserView[];
  offset: number;
  limit: number;
  query?: string;
}

export interface AdminListUsersQuery {
  offset?: number;
  limit?: number;
  // Matches a normalized email PREFIX, or an exact user id. Not a
  // substring: the server's match is anchored so it rides the unique
  // index on email.
  q?: string;
}

// ---- One account's file (GET /api/admin/users/{id}) ----

export interface AdminUserOrgView {
  org_id: string;
  org_name?: string;
  org_slug?: string;
  role: OrgRole;
  personal?: boolean;
  joined_at?: string;
}

export interface AdminUserTeamView {
  team_id: string;
  team_name?: string;
  team_slug?: string;
  org_id?: string;
  org_name?: string;
  role: Role;
  status?: string;
  personal?: boolean;
  joined_at?: string;
  // A team grant with no matching org membership. The invariant says it
  // cannot happen; when it does, this is the row to act on.
  orphan_grant?: boolean;
}

export interface AdminUserSSOLinkView {
  provider: string;
  subject: string;
  email?: string;
  created_at?: string;
}

export interface AdminUserDetail {
  user: UserView;
  // Whether a password sign-in is possible at all. With sso_links empty
  // too, the account has no way in — which is the shape a GitHub login
  // provisioned outside the SSO allow-list ends up with.
  has_password: boolean;
  orgs: AdminUserOrgView[];
  teams: AdminUserTeamView[];
  sso_links: AdminUserSSOLinkView[];
}

export function getAdminUser(userID: string): Promise<AdminUserDetail> {
  return guard404("admin-users", () =>
    request<AdminUserDetail>(`/admin/users/${encodeURIComponent(userID)}`),
  );
}

export interface AdminUpdateUserInput {
  status?: UserStatus;
  is_super_admin?: boolean;
  name?: string;
}

export function listAdminUsers(q: AdminListUsersQuery = {}): Promise<AdminUsersResponse> {
  const sp = new URLSearchParams();
  if (q.offset && q.offset > 0) sp.set("offset", String(q.offset));
  if (q.limit && q.limit > 0) sp.set("limit", String(q.limit));
  if (q.q && q.q.trim() !== "") sp.set("q", q.q.trim());
  const s = sp.toString();
  return guard404("admin-users", () =>
    request<AdminUsersResponse>(`/admin/users${s ? `?${s}` : ""}`),
  );
}

export function updateAdminUser(
  userID: string,
  input: AdminUpdateUserInput,
): Promise<UserView> {
  return guard404("admin-users", () =>
    request<UserView>(`/admin/users/${encodeURIComponent(userID)}`, {
      method: "PATCH",
      body: JSON.stringify(input),
    }),
  );
}

// Mints a one-shot temporary password for a locked-out account (shown
// once); the user's next sign-in goes through the forced-rotation flow
// with it. Super-admin only.
export function resetAdminUserPassword(
  userID: string,
): Promise<{ temp_password: string }> {
  return guard404("admin-users", () =>
    request<{ temp_password: string }>(
      `/admin/users/${encodeURIComponent(userID)}/reset-password`,
      { method: "POST" },
    ),
  );
}
