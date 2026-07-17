// Config-share editor client — ISOLATED from the operator API client
// (@/api/client) by design.
//
// This module is the ONLY network layer the shell-less /config/:id editor
// uses. It MUST NOT import from @/api/client because:
//   - the shared apiRequest sets credentials: "include" (sending the operator's
//     session cookie on every /api call), which would leak that cookie to the
//     public share endpoint and — worse — let the studio's global 401 handler
//     sign the operator out on any transient share error;
//   - the shared apiRequest also transparently attempts POST /auth/refresh on
//     401, rotating the operator's refresh token from a page an anonymous
//     visitor is holding.
//
// So every request here uses credentials: "omit" and sends the share token
// exclusively via `Authorization: Bearer <token>`. There is no shared 401
// handler, no refresh, no cookie surface. An eslint rule
// (no-restricted-imports, scoped to src/views/ConfigShare/**) enforces the
// boundary at build time; keeping this module import-clean is the other half.
//
// Base URL: the shared client derives its /api prefix from apiBase() in
// @/lib/scope. We DO NOT reuse that helper — we replicate its narrow contract
// (VITE_API_URL override else "/api", plus a workspace-pane /x/<id> prefix)
// inline so this file has zero application-layer dependencies and can't
// accidentally pick up scope-side effects (WS ticket cache, etc.).

// Base URL derivation, replicated from @/lib/scope apiBase() so we don't
// import it — same contract, zero cross-coupling.
function shareApiBase(): string {
  const scope = (globalThis as { __ITERION_SCOPE__?: unknown }).__ITERION_SCOPE__;
  const prefix = typeof scope === "string" && scope.startsWith("/x/") ? scope : "";
  const configured = import.meta.env.VITE_API_URL ?? "/api";
  return prefix + configured;
}

// ShareMeta mirrors GET /api/config-share/:id/meta. Fields track the Go
// map[string]any writer in pkg/server/config_share_routes.go.
export interface ShareMeta {
  bot_id: string;
  label: string;
  config_path: string;
  category: string;
  schema_ref: string;
  allowed_paths: string[];
  visible_paths: string[];
  read_only: boolean;
}

// ShareConfigResponse mirrors GET /api/config-share/:id/config. `config`
// is the file projected to visible_paths only.
export interface ShareConfigResponse {
  config: Record<string, unknown>;
  sha: string;
}

// SharePatchSuccess mirrors 200 PATCH /api/config-share/:id/config.
export interface SharePatchSuccess {
  sha: string;
  changed: string[];
}

// SharePatchConflict is the 409 body — returned as a normal typed value
// (not a throw) so the caller can render the fresh projection alongside
// the user's draft. NEVER auto-retry.
export interface SharePatchConflict {
  kind: "conflict";
  config: Record<string, unknown>;
  sha: string;
}

export type SharePatchResult =
  | { kind: "ok"; sha: string; changed: string[] }
  | SharePatchConflict;

// ShareApiError is thrown for every non-2xx, non-409 response so the caller
// can react to expired/invalid tokens (401), not-editable-field (400) etc.
// 401 is a first-class state — the editor must render the "invalid or
// expired share" screen; there is no refresh/retry.
export class ShareApiError extends Error {
  status: number;
  body: string;
  constructor(status: number, message: string, body: string) {
    super(message);
    this.name = "ShareApiError";
    this.status = status;
    this.body = body;
  }
}

// Envelope shared by every server error path (`httpError` in pkg/server).
async function readErrorMessage(res: Response): Promise<{ short: string; raw: string }> {
  const raw = await res.text();
  if (!raw) return { short: res.statusText || `HTTP ${res.status}`, raw: "" };
  try {
    const obj = JSON.parse(raw) as { error?: unknown; message?: unknown };
    if (typeof obj.error === "string" && obj.error) return { short: obj.error, raw };
    if (typeof obj.message === "string" && obj.message) return { short: obj.message, raw };
  } catch {
    // fall through to raw
  }
  return { short: raw.slice(0, 200), raw };
}

// shareFetch is the single choke point for every share request. It builds
// the URL, pins credentials: "omit", attaches Bearer auth, and normalises
// error responses.
async function shareFetch(path: string, token: string, init?: RequestInit): Promise<Response> {
  const base = shareApiBase();
  const url = `${base}${path}`;
  const headers = new Headers(init?.headers ?? {});
  headers.set("Authorization", `Bearer ${token}`);
  if (init?.body !== undefined && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }
  // credentials: "omit" is load-bearing — it prevents the operator's session
  // cookie from being attached on the same origin.
  return fetch(url, {
    ...init,
    headers,
    credentials: "omit",
  });
}

async function throwOnError(res: Response, op: string): Promise<void> {
  const { short, raw } = await readErrorMessage(res);
  throw new ShareApiError(res.status, `${op}: ${short}`, raw);
}

// getShareMeta returns the operator-provisioned scope for this share:
// which bot / repo / category is being edited and which JSON paths the
// editor may read (visible_paths) vs write (allowed_paths).
export async function getShareMeta(shareID: string, token: string): Promise<ShareMeta> {
  const res = await shareFetch(`/config-share/${encodeURIComponent(shareID)}/meta`, token);
  if (!res.ok) await throwOnError(res, "meta");
  return (await res.json()) as ShareMeta;
}

// getShareConfig fetches the visible-path projection of the file plus the
// optimistic-concurrency SHA. Send the same SHA back on PATCH so the server
// can 409 on any race.
export async function getShareConfig(shareID: string, token: string): Promise<ShareConfigResponse> {
  const res = await shareFetch(`/config-share/${encodeURIComponent(shareID)}/config`, token);
  if (!res.ok) await throwOnError(res, "read config");
  return (await res.json()) as ShareConfigResponse;
}

// patchShareConfig submits a nested-object patch containing ONLY the edited
// leaves under an allowed path. On 200 returns the new SHA + list of changed
// dotted paths; on 409 returns the fresh server-side projection so the caller
// can render a conflict diff.
export async function patchShareConfig(
  shareID: string,
  token: string,
  patch: Record<string, unknown>,
  sha: string,
): Promise<SharePatchResult> {
  const res = await shareFetch(`/config-share/${encodeURIComponent(shareID)}/config`, token, {
    method: "PATCH",
    body: JSON.stringify({ patch, sha }),
  });
  if (res.status === 409) {
    const body = (await res.json()) as { config: Record<string, unknown>; sha: string };
    return { kind: "conflict", config: body.config, sha: body.sha };
  }
  if (!res.ok) await throwOnError(res, "patch config");
  const body = (await res.json()) as SharePatchSuccess;
  return { kind: "ok", sha: body.sha, changed: body.changed };
}
