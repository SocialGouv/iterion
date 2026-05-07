// BYOK + OAuth-forfait API client.

const BASE = (import.meta.env.VITE_API_URL ?? "/api").replace(/\/$/, "");

export type Provider =
  | "anthropic"
  | "openai"
  | "bedrock"
  | "vertex"
  | "azure"
  | "openrouter"
  | "xai";

export interface ApiKeyView {
  id: string;
  provider: Provider;
  name: string;
  last4?: string;
  fingerprint?: string;
  is_default: boolean;
  scope_user_id?: string;
  created_at: string;
  last_used_at?: string;
}

export type OAuthKind = "claude_code" | "codex";

export interface OAuthConnection {
  kind: OAuthKind;
  scopes?: string[];
  access_token_expires_at?: string;
  last_refreshed_at?: string;
  created_at: string;
  updated_at: string;
}

async function send<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`${BASE}${path}`, {
    credentials: "include",
    headers: init?.body ? { "Content-Type": "application/json", ...(init?.headers ?? {}) } : init?.headers,
    ...init,
  });
  if (!res.ok) {
    let body: any = null;
    try {
      body = await res.json();
    } catch {}
    const msg = body?.error ?? body?.message ?? res.statusText;
    throw new Error(`${msg || `HTTP ${res.status}`}`);
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

// ---- Team-scoped BYOK ----

export async function listTeamApiKeys(teamID: string): Promise<ApiKeyView[]> {
  const res = await send<{ keys: ApiKeyView[] }>(`/teams/${encodeURIComponent(teamID)}/api-keys`);
  return res.keys ?? [];
}

export async function createTeamApiKey(teamID: string, input: {
  provider: Provider;
  name: string;
  secret: string;
  is_default?: boolean;
}): Promise<ApiKeyView> {
  return send(`/teams/${encodeURIComponent(teamID)}/api-keys`, {
    method: "POST",
    body: JSON.stringify(input),
  });
}

// ---- User-scoped BYOK ----

export async function listMyApiKeys(): Promise<ApiKeyView[]> {
  const res = await send<{ keys: ApiKeyView[] }>(`/me/api-keys`);
  return res.keys ?? [];
}

export async function createMyApiKey(input: {
  provider: Provider;
  name: string;
  secret: string;
  is_default?: boolean;
}): Promise<ApiKeyView> {
  return send(`/me/api-keys`, {
    method: "POST",
    body: JSON.stringify(input),
  });
}

// ---- Mutations shared between team + user keys ----

export async function updateApiKey(
  scope: { team_id: string } | { mine: true },
  keyID: string,
  input: { name?: string; is_default?: boolean; secret?: string },
): Promise<ApiKeyView> {
  const root = "team_id" in scope
    ? `/teams/${encodeURIComponent(scope.team_id)}/api-keys/${encodeURIComponent(keyID)}`
    : `/me/api-keys/${encodeURIComponent(keyID)}`;
  return send(root, {
    method: "PATCH",
    body: JSON.stringify(input),
  });
}

export async function deleteApiKey(
  scope: { team_id: string } | { mine: true },
  keyID: string,
): Promise<void> {
  const root = "team_id" in scope
    ? `/teams/${encodeURIComponent(scope.team_id)}/api-keys/${encodeURIComponent(keyID)}`
    : `/me/api-keys/${encodeURIComponent(keyID)}`;
  await send(root, { method: "DELETE" });
}

// ---- OAuth-forfait (per-user) ----

export async function listOAuthConnections(): Promise<OAuthConnection[]> {
  const res = await send<{ connections: OAuthConnection[] }>(`/me/oauth/connections`);
  return res.connections ?? [];
}

export async function uploadOAuthCredentials(kind: OAuthKind, blob: string): Promise<OAuthConnection> {
  return send(`/me/oauth/${encodeURIComponent(kind)}/credentials`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: blob,
  });
}

export async function refreshOAuth(kind: OAuthKind): Promise<OAuthConnection> {
  return send(`/me/oauth/${encodeURIComponent(kind)}/refresh`, { method: "POST" });
}

export async function deleteOAuth(kind: OAuthKind): Promise<void> {
  await send(`/me/oauth/${encodeURIComponent(kind)}`, { method: "DELETE" });
}

// ---- Team management (a thin slice — full surface lives elsewhere) ----

export interface TeamMemberView {
  user_id: string;
  email?: string;
  name?: string;
  role: "owner" | "admin" | "member" | "viewer";
}

export async function listTeamMembers(teamID: string): Promise<TeamMemberView[]> {
  const res = await send<{ members: TeamMemberView[] }>(`/teams/${encodeURIComponent(teamID)}/members`);
  return res.members ?? [];
}

export interface InvitationView {
  id: string;
  email: string;
  role: string;
  team_id: string;
  expires_at: string;
  accepted_at?: string;
}

export async function listInvitations(teamID: string): Promise<InvitationView[]> {
  const res = await send<{ invitations: InvitationView[] }>(`/teams/${encodeURIComponent(teamID)}/invitations`);
  return res.invitations ?? [];
}

export async function createInvitation(teamID: string, input: { email: string; role: string }): Promise<{
  id: string;
  token: string;
  email: string;
  role: string;
  expires_at: string;
}> {
  return send(`/teams/${encodeURIComponent(teamID)}/invitations`, {
    method: "POST",
    body: JSON.stringify(input),
  });
}

export async function deleteInvitation(teamID: string, invID: string): Promise<void> {
  await send(`/teams/${encodeURIComponent(teamID)}/invitations/${encodeURIComponent(invID)}`, {
    method: "DELETE",
  });
}

export async function updateMemberRole(teamID: string, userID: string, role: string): Promise<TeamMemberView> {
  return send(`/teams/${encodeURIComponent(teamID)}/members/${encodeURIComponent(userID)}`, {
    method: "PATCH",
    body: JSON.stringify({ role }),
  });
}

export async function removeMember(teamID: string, userID: string): Promise<void> {
  await send(`/teams/${encodeURIComponent(teamID)}/members/${encodeURIComponent(userID)}`, {
    method: "DELETE",
  });
}
