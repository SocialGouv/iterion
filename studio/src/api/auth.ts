// Auth API client. Mirrors the Go handlers in pkg/server/auth_routes.go.
//
// The server reads/writes a HttpOnly auth cookie + a refresh cookie
// scoped to /api/auth, so most calls do NOT need to pass tokens
// explicitly. We send `credentials: "include"` on every request so
// cross-origin dev (vite proxy) still attaches them.

import { apiBase } from "@/lib/scope";

const BASE = apiBase().replace(/\/$/, "");

// `config_editor` is a least-privilege team role: it can ONLY edit the team's
// config-shares (via the ConfigEditorShell), nothing else in the studio. It
// sits below `viewer` in the access ladder (a viewer reads the whole studio;
// a config_editor reaches only the config editor).
export type Role = "owner" | "admin" | "member" | "viewer" | "config_editor";
export type UserStatus = "active" | "disabled" | "pending_password_change";

export interface UserView {
  id: string;
  email: string;
  name?: string;
  status: UserStatus;
  is_super_admin: boolean;
  created_at?: string;
}

export type OrgRole = "owner" | "admin" | "member";

export interface MembershipView {
  team_id: string;
  team_name: string;
  team_slug: string;
  role: Role;
  personal?: boolean;
}

// OrgTreeView is one organization the user belongs to, carrying the
// teams inside it they can access. /api/auth/me returns these so the SPA
// can render an Org picker → Team picker with no extra round trips.
export interface OrgTreeView {
  org_id: string;
  org_name: string;
  org_slug: string;
  org_role: OrgRole | "";
  personal?: boolean;
  teams: MembershipView[];
}

export interface AuthResponse {
  user: UserView;
  orgs: OrgTreeView[];
  active_org_id?: string;
  active_org_role?: OrgRole | "";
  active_team_id?: string;
  active_role?: Role | "";
  access_token?: string;
  expires_at?: string;
}

export interface ProvidersResponse {
  signup_mode: "open" | "invite_only";
  providers: Array<{ name: string; display: string }>;
}

interface ApiErrorBody {
  error?: string;
  message?: string;
  [k: string]: unknown;
}

async function send<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`${BASE}${path}`, {
    credentials: "include",
    headers: { "Content-Type": "application/json", ...(init?.headers ?? {}) },
    ...init,
  });
  if (!res.ok) {
    let body: ApiErrorBody | null = null;
    try {
      body = (await res.json()) as ApiErrorBody;
    } catch {
      // body wasn't JSON; fall back to text
    }
    const msg = body?.error ?? body?.message ?? res.statusText;
    const err = new ApiError(msg || `HTTP ${res.status}`, res.status, body);
    throw err;
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

export class ApiError extends Error {
  status: number;
  body: ApiErrorBody | null;
  constructor(message: string, status: number, body?: ApiErrorBody | null) {
    super(message);
    this.status = status;
    this.body = body ?? null;
  }
}

export async function login(email: string, password: string): Promise<AuthResponse> {
  return send("/auth/login", {
    method: "POST",
    body: JSON.stringify({ email, password }),
  });
}

export async function register(input: {
  email: string;
  password: string;
  name?: string;
  invitation?: string;
}): Promise<AuthResponse> {
  return send("/auth/register", {
    method: "POST",
    body: JSON.stringify(input),
  });
}

export async function refresh(): Promise<AuthResponse> {
  return send("/auth/refresh", { method: "POST" });
}

export async function logout(): Promise<void> {
  await send("/auth/logout", { method: "POST" });
}

export async function getMe(): Promise<AuthResponse> {
  return send("/auth/me");
}

export async function switchTeam(teamID: string): Promise<AuthResponse> {
  return send(`/auth/me/team/${encodeURIComponent(teamID)}`, { method: "POST" });
}

// switchOrg changes the active organization. The server picks an active
// team within the new org and returns the full org tree + active context.
export async function switchOrg(orgID: string): Promise<AuthResponse> {
  return send(`/auth/me/org/${encodeURIComponent(orgID)}`, { method: "POST" });
}

// listProviders returns the global SSO providers, plus — when an email or org
// slug is supplied — that org's own enabled providers (its Keycloak), resolved
// from the email's verified domain or the explicit slug. An unknown
// email/slug returns only the globals (no org-existence oracle), so this is
// safe to call speculatively as the user types.
export async function listProviders(opts?: {
  email?: string;
  org?: string;
}): Promise<ProvidersResponse> {
  const p = new URLSearchParams();
  if (opts?.email) p.set("email", opts.email);
  if (opts?.org) p.set("org", opts.org);
  const qs = p.toString();
  return send(`/auth/providers${qs ? `?${qs}` : ""}`);
}

// ---- Connected SSO identities (settings) ----

export interface SSOLink {
  provider: string;
  provider_user_id: string;
  email?: string;
  created_at?: string;
}

export async function listSSOLinks(): Promise<{ links: SSOLink[] }> {
  return send("/me/sso/links");
}

export async function unlinkSSO(
  provider: string,
  providerUserID: string,
): Promise<void> {
  await send(
    `/me/sso/links/${encodeURIComponent(provider)}/${encodeURIComponent(providerUserID)}`,
    { method: "DELETE" },
  );
}

// ssoLinkStartURL is the top-level navigation that begins connecting a new SSO
// identity to the signed-in account (the exit from the 409 link-required
// dead-end). It's a GET redirect, so the caller assigns window.location.
export function ssoLinkStartURL(provider: string): string {
  return `${BASE}/me/sso/${encodeURIComponent(provider)}/link/start`;
}

export interface InvitationLookup {
  email: string;
  role: Role;
  team_id: string;
  team_name: string;
}

export async function lookupInvitation(token: string): Promise<InvitationLookup> {
  const params = new URLSearchParams({ token });
  return send(`/auth/invitations/lookup?${params.toString()}`);
}

export async function acceptInvitationLoggedIn(token: string): Promise<MembershipView> {
  return send("/auth/invitations/accept", {
    method: "POST",
    body: JSON.stringify({ token }),
  });
}

// ---- Password flows ----

// Anti-enumeration: this endpoint always returns 200 with the same body
// regardless of whether the email matches an account. The view shows a
// generic "If we have an account for that email, you'll receive a link"
// confirmation regardless of outcome.
export async function requestPasswordReset(email: string): Promise<void> {
  await send("/auth/password/reset/request", {
    method: "POST",
    body: JSON.stringify({ email }),
  });
}

// Redeems the emailed token: new password is set, every prior session is
// revoked, and a fresh login is issued (cookies set).
export async function confirmPasswordReset(
  token: string,
  password: string,
): Promise<AuthResponse> {
  return send("/auth/password/reset/confirm", {
    method: "POST",
    body: JSON.stringify({ token, password }),
  });
}

// Forced rotation for a pending_password_change account (e.g. the
// bootstrapped super-admin). Public + anti-enumeration: a bad email/temp
// password is opaquely 401.
export async function completePendingPasswordChange(
  email: string,
  currentPassword: string,
  newPassword: string,
): Promise<AuthResponse> {
  return send("/auth/password/change", {
    method: "POST",
    body: JSON.stringify({
      email,
      current_password: currentPassword,
      new_password: newPassword,
    }),
  });
}

// Authenticated self-service rotation. Server revokes other sessions
// and re-issues the calling session's cookies.
export async function changeMyPassword(
  currentPassword: string,
  newPassword: string,
): Promise<AuthResponse> {
  return send("/me/password", {
    method: "POST",
    body: JSON.stringify({
      current_password: currentPassword,
      new_password: newPassword,
    }),
  });
}

// "Sign out everywhere": every live refresh dies now; the caller's own
// cookies are cleared too.
export async function revokeAllMySessions(): Promise<void> {
  await send("/me/sessions/revoke-all", { method: "POST" });
}
