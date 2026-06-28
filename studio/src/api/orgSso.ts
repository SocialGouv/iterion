import { guard404, request } from "./client";

// Per-tenant SSO providers — a team's own Keycloak (kind "oidc") and the
// GitHub team allow-list (kind "github"). Mirrors pkg/auth/orgsso +
// pkg/server/org_sso_routes.go. Distinct from api/auth.ts (the login flow).

export type OrgSSOKind = "oidc" | "github";
export type Role = "owner" | "admin" | "member" | "viewer";

export interface GitHubTeamGrant {
  github_org: string;
  github_org_id?: number;
  team_slug?: string;
  team_id?: number;
  role: Role;
  /** Set once the org's control of github_org is verified (a tracked
   * follow-up); until then a grant is active but flagged "unverified". */
  verified: boolean;
}

export interface OrgSSOProvider {
  id: string;
  tenant_id: string;
  kind: OrgSSOKind;
  enabled: boolean;
  display_name?: string;
  // kind "oidc"
  issuer_url?: string;
  client_id?: string;
  scopes?: string[];
  default_role?: Role;
  auto_link_on_email?: boolean;
  // kind "github"
  grants?: GitHubTeamGrant[];
  auto_provision?: boolean;
  // response-only: the redirect URI to register at the IdP (oidc rows).
  redirect_uri?: string;
  created_at: string;
  updated_at: string;
}

export interface OrgSSOProviderInput {
  kind: OrgSSOKind;
  display_name?: string;
  enabled?: boolean;
  // oidc
  issuer_url?: string;
  client_id?: string;
  /** Write-only; empty on update keeps the stored secret. */
  client_secret?: string;
  scopes?: string[];
  default_role?: Role;
  auto_link_on_email?: boolean;
  // github
  auto_provision?: boolean;
  grants?: GitHubTeamGrant[];
}

// OrgDomain is a tenant's email-domain claim, proven via a DNS TXT challenge.
// A verified domain gates per-org SSO auto-link (an org's IdP may auto-link
// addresses only at domains it has proven it controls).
export interface OrgDomain {
  id: string;
  tenant_id: string;
  domain: string;
  token: string;
  verified_at?: string;
  /** DNS record name + value the admin must publish to verify ownership. */
  challenge_host: string;
  challenge_value: string;
  created_at: string;
}

export async function listOrgDomains(teamID: string): Promise<OrgDomain[]> {
  const r = await guard404("org_sso", () =>
    request<{ domains: OrgDomain[] }>(`/orgs/${teamID}/sso/domains`),
  );
  return r.domains ?? [];
}

export async function addOrgDomain(teamID: string, domain: string): Promise<OrgDomain> {
  return request(`/orgs/${teamID}/sso/domains`, {
    method: "POST",
    body: JSON.stringify({ domain }),
  });
}

export async function verifyOrgDomain(
  teamID: string,
  domainID: string,
): Promise<{ verified: boolean; error?: string }> {
  return request(`/orgs/${teamID}/sso/domains/${domainID}/verify`, { method: "POST" });
}

export async function deleteOrgDomain(teamID: string, domainID: string): Promise<void> {
  await request<void>(`/orgs/${teamID}/sso/domains/${domainID}`, { method: "DELETE" });
}

export async function listOrgSSOProviders(teamID: string): Promise<OrgSSOProvider[]> {
  const r = await guard404("org_sso", () =>
    request<{ providers: OrgSSOProvider[] }>(`/orgs/${teamID}/sso/providers`),
  );
  return r.providers ?? [];
}

export async function createOrgSSOProvider(
  teamID: string,
  input: OrgSSOProviderInput,
): Promise<OrgSSOProvider> {
  return request(`/orgs/${teamID}/sso/providers`, {
    method: "POST",
    body: JSON.stringify(input),
  });
}

export async function updateOrgSSOProvider(
  teamID: string,
  providerID: string,
  input: OrgSSOProviderInput,
): Promise<OrgSSOProvider> {
  return request(`/orgs/${teamID}/sso/providers/${providerID}`, {
    method: "PATCH",
    body: JSON.stringify(input),
  });
}

export async function deleteOrgSSOProvider(teamID: string, providerID: string): Promise<void> {
  await request<void>(`/orgs/${teamID}/sso/providers/${providerID}`, { method: "DELETE" });
}

export async function testOrgSSOProvider(
  teamID: string,
  providerID: string,
): Promise<{ ok: boolean; error?: string }> {
  return request(`/orgs/${teamID}/sso/providers/${providerID}/test`, { method: "POST" });
}
