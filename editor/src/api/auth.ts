// Auth API client. Mirrors the Go handlers in pkg/server/auth_routes.go.
//
// The server reads/writes a HttpOnly auth cookie + a refresh cookie
// scoped to /api/auth, so most calls do NOT need to pass tokens
// explicitly. We send `credentials: "include"` on every request so
// cross-origin dev (vite proxy) still attaches them.

const BASE = (import.meta.env.VITE_API_URL ?? "/api").replace(/\/$/, "");

export type Role = "owner" | "admin" | "member" | "viewer";
export type UserStatus = "active" | "disabled" | "pending_password_change";

export interface UserView {
  id: string;
  email: string;
  name?: string;
  status: UserStatus;
  is_super_admin: boolean;
  created_at?: string;
}

export interface MembershipView {
  team_id: string;
  team_name: string;
  team_slug: string;
  role: Role;
  personal?: boolean;
}

export interface AuthResponse {
  user: UserView;
  teams: MembershipView[];
  active_team_id?: string;
  active_role?: Role | "";
  access_token?: string;
  expires_at?: string;
}

export interface ProvidersResponse {
  signup_mode: "open" | "invite_only";
  providers: Array<{ name: string; display: string }>;
}

async function send<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`${BASE}${path}`, {
    credentials: "include",
    headers: { "Content-Type": "application/json", ...(init?.headers ?? {}) },
    ...init,
  });
  if (!res.ok) {
    let body: any = null;
    try {
      body = await res.json();
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
  body: any;
  constructor(message: string, status: number, body?: any) {
    super(message);
    this.status = status;
    this.body = body;
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

export async function listProviders(): Promise<ProvidersResponse> {
  return send("/auth/providers");
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
