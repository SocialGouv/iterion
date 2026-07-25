// Super-admin user console — REST client. Mirrors auth_routes.go's
// /api/admin/users handlers.

import { FeatureUnavailableError, guard404, request } from "./client";
import type { UserStatus, UserView } from "./auth";

export { FeatureUnavailableError };

export interface AdminUsersResponse {
  users: UserView[];
  offset: number;
  limit: number;
}

export interface AdminListUsersQuery {
  offset?: number;
  limit?: number;
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
