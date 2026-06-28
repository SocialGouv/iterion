// Org roster API: members + invitations at the ORGANIZATION level
// (the billing/SSO identity). Team-level access grants are separate
// (api/byok.ts → /teams/{id}/members).
import { request } from "./client";
import type { OrgRole, Role } from "./auth";

export interface OrgMemberView {
  user_id: string;
  email?: string;
  name?: string;
  role: OrgRole;
}

export interface OrgInvitationView {
  id: string;
  email: string;
  role: Role;
  team_id?: string;
  expires_at: string;
}

export async function listOrgMembers(orgID: string): Promise<OrgMemberView[]> {
  const r = await request<{ members: OrgMemberView[] }>(
    `/orgs/${encodeURIComponent(orgID)}/members`,
  );
  return r.members ?? [];
}

export async function updateOrgMemberRole(
  orgID: string,
  userID: string,
  role: OrgRole,
): Promise<void> {
  await request(`/orgs/${encodeURIComponent(orgID)}/members/${encodeURIComponent(userID)}`, {
    method: "PATCH",
    body: JSON.stringify({ role }),
  });
}

export async function removeOrgMember(orgID: string, userID: string): Promise<void> {
  await request<void>(
    `/orgs/${encodeURIComponent(orgID)}/members/${encodeURIComponent(userID)}`,
    { method: "DELETE" },
  );
}

export async function listOrgInvitations(orgID: string): Promise<OrgInvitationView[]> {
  const r = await request<{ invitations: OrgInvitationView[] }>(
    `/orgs/${encodeURIComponent(orgID)}/invitations`,
  );
  return r.invitations ?? [];
}

export async function createOrgInvitation(
  orgID: string,
  input: { email: string; role: string; team_id?: string },
): Promise<{ id: string; token: string; email: string; role: string }> {
  return request(`/orgs/${encodeURIComponent(orgID)}/invitations`, {
    method: "POST",
    body: JSON.stringify(input),
  });
}

export async function deleteOrgInvitation(orgID: string, inviteID: string): Promise<void> {
  await request<void>(
    `/orgs/${encodeURIComponent(orgID)}/invitations/${encodeURIComponent(inviteID)}`,
    { method: "DELETE" },
  );
}
