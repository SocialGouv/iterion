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
  updated_at?: string;
  // Absent when the account has never completed a sign-in — which is a
  // different state from "dormant", and the one that distinguishes an
  // account that cannot get in from one that simply has not lately.
  last_login_at?: string;
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
  unavailable?: boolean;
}

// AuthIdentity is GET /api/auth/me: the account, its org→teams tree and the
// active context. /api/auth/me is the one route that answers the tree.
export interface AuthIdentity {
  user: UserView;
  orgs: OrgTreeView[];
  active_org_id?: string;
  active_org_role?: OrgRole | "";
  active_team_id?: string;
  active_role?: Role | "";
  // The refresh session the presented access token belongs to. Absent when
  // the credential names no session — a personal access token, an access
  // token minted before sessions carried a lineage, local/dev mode — and a
  // sign-out then ends whatever session the browser holds, as it always did.
  // What it buys: a pending sign-out is bound to THIS session, so a logout
  // presented later (a reload, the landing of a sign-in made since) ends the
  // session it was asked about and no other.
  session_lineage?: string;
  // That session's bound, as the server holds it — never the access token's
  // short-lived exp. A pending sign-out's record names it: the notices show
  // a date instead of "its expiry", and the machine drops the record past it
  // (nothing live is being hidden). Absent for a credential that names no session, and when the session is gone.
  expires_at?: string;
}

// AuthSession is what every route that mints a credential answers (login,
// register, refresh, password change or reset, team/org switch): the
// account and the active context of the new access token — never the org
// tree, which is read from getMe().
export interface AuthSession {
  user: UserView;
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

export async function login(email: string, password: string): Promise<AuthSession> {
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
}): Promise<AuthSession> {
  return send("/auth/register", {
    method: "POST",
    body: JSON.stringify(input),
  });
}

// The refresh and the logout are not here: they belong to the client module
// (@/api/client), which owns the session's lifecycle — every presentation of
// the refresh token goes through its one guarded path, and the logout through
// its bounded sign-out.

// KEYCHAIN_RETRY_HEADER asks a desktop connection to read the system keychain
// NOW, whatever back-off it is on: the user pressed the retry of the
// keychain-locked screen, so the unlock prompt this read raises is one they
// asked for. Only a user action may set it — an automatic read carrying it
// would be the prompt loop the back-off exists to stop.
export const KEYCHAIN_RETRY_HEADER = "X-Iterion-Keychain-Retry";

// KEYCHAIN_LOCKED_CODE is the error code a desktop connection answers (503)
// when it could not read the token because the system keychain is locked.
export const KEYCHAIN_LOCKED_CODE = "keychain_locked";

export interface GetMeOptions {
  // keychainRetry sends KEYCHAIN_RETRY_HEADER: a user-driven retry of a
  // connection whose keychain is locked.
  keychainRetry?: boolean;
}

export async function getMe(options?: GetMeOptions): Promise<AuthIdentity> {
  return send("/auth/me", options?.keychainRetry ? { headers: { [KEYCHAIN_RETRY_HEADER]: "1" } } : undefined);
}

// errorCodeOf is the stable machine code of an error envelope, "" when there
// is none. The envelope carries it as `error_code` — the key every coded
// refusal of this API uses — and it is what a caller branches on instead of
// matching the human message, which is free to be rewritten.
export function errorCodeOf(err: unknown): string {
  if (!(err instanceof ApiError)) return "";
  return typeof err.body?.error_code === "string" ? err.body.error_code : "";
}

// PASSWORD_CHANGED_SIGN_IN_CODE is the 409 a password change answers when the
// new password IS committed but no session could be opened for it: the auth
// cookies are cleared, so the browser is signed out and the user signs in with
// the new password. Retrying the same request is pointless — the old password
// it carries is no longer the account's.
export const PASSWORD_CHANGED_SIGN_IN_CODE = "password_changed_sign_in";

// SIGNOUT_CLEANUP_FAILED_CODE is the 503 a "sign out everywhere" answers when
// a cleanup step failed AFTER the sign-out landed: the sessions are ended and
// the caller's cookies are cleared, but something on the account may be left
// half-done. The studio renders its own actionable text for it (the user must
// sign in again and repeat the sign-out everywhere) instead of the plain
// transport message.
export const SIGNOUT_CLEANUP_FAILED_CODE = "signout_cleanup_failed";

export const SIGNOUT_CLEANUP_FAILED_TEXT =
  "Sign-out done; cleanup incomplete — sign in again and repeat sign out everywhere.";

// CREDENTIAL_CHANGE_BUSY_CODE is the other 409 a password change answers:
// another change of the same account is in progress. Nothing happened, the
// password is unchanged, and the user retries — so it must NOT be read as
// "the password changed", which is why the code decides and not the status.
export const CREDENTIAL_CHANGE_BUSY_CODE = "credential_change_busy";

// passwordChangedSignIn answers the server's message when err is the 409 that
// COMMITTED the new password without a session, and null otherwise — a 409
// credential_change_busy included. One helper for the three forms that change
// a password, so the next one cannot forget the case or confuse the two.
export function passwordChangedSignIn(err: unknown): string | null {
  if (!(err instanceof ApiError) || err.status !== 409) return null;
  if (errorCodeOf(err) !== PASSWORD_CHANGED_SIGN_IN_CODE) return null;
  // send() already puts the envelope's `error` on the message.
  return err.message;
}

// switchTeam and switchOrg mint a credential (the access cookie of the new
// context); the signal is the sign-out's, which cancels a switch in flight so
// its answer never sets that cookie after the logout cleared the jar.
export async function switchTeam(teamID: string, signal?: AbortSignal): Promise<AuthSession> {
  return send(`/auth/me/team/${encodeURIComponent(teamID)}`, { method: "POST", signal });
}

// switchOrg changes the active organization. The server picks an active
// team within the new org and returns the new active context.
export async function switchOrg(orgID: string, signal?: AbortSignal): Promise<AuthSession> {
  return send(`/auth/me/org/${encodeURIComponent(orgID)}`, { method: "POST", signal });
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
  // True when a signed-in session wrote the link from the account settings —
  // the links "Sign out everywhere" and an admin disable remove, so an
  // account never loses the sign-in method it started with (a passwordless
  // account whose every link matches keeps its oldest). The link the
  // sign-out KEEPS is the one without the flag. Absent from an older server:
  // the distinction is then unknown and shown as such.
  session_linked?: boolean;
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
): Promise<AuthSession> {
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
): Promise<AuthSession> {
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
): Promise<AuthSession> {
  return send("/me/password", {
    method: "POST",
    body: JSON.stringify({
      current_password: currentPassword,
      new_password: newPassword,
    }),
  });
}

// "Sign out everywhere" is the compromise response: every live refresh session
// dies now, every personal access token of the account is revoked (the CLI's
// and CI's included), and the caller's own cookies are cleared too. A password
// change, an emailed reset and an admin password reset keep the tokens — this
// route is the one that does not.
export async function revokeAllMySessions(): Promise<void> {
  await send("/me/sessions/revoke-all", { method: "POST" });
}
