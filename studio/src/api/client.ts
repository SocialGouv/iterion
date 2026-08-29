import type { IterDocument, FileEntry, ListFilesResponse, SaveFileResponse } from "./types";
import { apiBase, isScopedPane, scopePrefix } from "@/lib/scope";

const BASE_URL = apiBase();

// onUnauthorized fires when the studio server returns 401 on any
// /api/* call AND a token refresh couldn't recover the session. The
// AuthProvider registers a handler that flips its state to `anonymous`
// so the App swaps in the Login view.
let onUnauthorized: (() => void) | null = null;

export function setUnauthorizedHandler(fn: (() => void) | null) {
  onUnauthorized = fn;
}

// refreshInFlight dedupes concurrent refresh attempts: when several calls
// 401 at once (the access JWT just expired), they all await the same single
// POST /auth/refresh instead of racing — refresh rotates the refresh token,
// so concurrent rotations would invalidate each other and bounce the user to
// login. Resolves true when the session was renewed.
let refreshInFlight: Promise<boolean> | null = null;

function tryRefreshSession(): Promise<boolean> {
  if (!refreshInFlight) {
    refreshInFlight = fetch(`${BASE_URL}/auth/refresh`, {
      method: "POST",
      credentials: "include",
    })
      .then((r) => r.ok)
      .catch(() => false)
      .finally(() => {
        refreshInFlight = null;
      });
  }
  return refreshInFlight;
}

// ApiError is the typed error thrown by request/apiRequest when the
// HTTP status is non-2xx. Domain clients use `err instanceof ApiError
// && err.status === N` to react to specific statuses (notably 404 for
// feature-gating via `guard404`). The message keeps the historical
// `API error <status>: <body>` shape so toasts read the same as
// before.
export class ApiError extends Error {
  status: number;
  errorCode?: string;
  constructor(status: number, message: string, errorCode?: string) {
    super(message);
    this.status = status;
    this.errorCode = errorCode;
    this.name = "ApiError";
  }
}

// is404 reports whether a thrown value represents an HTTP 404. Prefers
// the typed ApiError.status check; the message-substring fallback exists
// because a few fetchers (fetchToolBlob's direct fetch, and any older
// path not yet routed through apiRequest) still throw plain Errors that
// only carry the historical "API error <status>: …" message shape.
// Remove the fallback once every fetcher throws ApiError.
export function is404(err: unknown): boolean {
  if (err instanceof ApiError) return err.status === 404;
  return err instanceof Error && err.message.includes("API error 404");
}

// FeatureUnavailableError marks a server endpoint that is not enabled
// on the current deployment (HTTP 404 on a domain route). Views catch
// it and render an EmptyState "Not enabled on this server" instead of
// crashing. Detection is class-based (instanceof) so the guard is
// robust against minified error messages.
export class FeatureUnavailableError extends Error {
  feature: string;
  constructor(feature: string, message?: string) {
    super(message ?? `${feature} not available on this server`);
    this.feature = feature;
    this.name = "FeatureUnavailableError";
  }
}

// guard404 wraps a domain request and converts an ApiError(404) into
// a typed FeatureUnavailableError so the calling view can show the
// "not enabled" empty state. Any other error type or status is
// rethrown unchanged.
export async function guard404<T>(feature: string, fn: () => Promise<T>): Promise<T> {
  try {
    return await fn();
  } catch (err) {
    if (err instanceof ApiError && err.status === 404) {
      throw new FeatureUnavailableError(feature, err.message);
    }
    throw err;
  }
}

// request is exported so other api/*.ts modules (api/projects.ts,
// future per-domain clients) share the same 401-handling and JSON-
// decoding semantics. It prefixes BASE_URL on the supplied path.
export async function request<T>(path: string, init?: RequestInit): Promise<T> {
  return apiRequest<T>(`${BASE_URL}${path}`, init);
}

// apiRequest is the same fetch wrapper but takes a fully-qualified
// path. Used by /api/v1/dispatcher/* and /api/v1/native/* clients that
// don't sit under the BASE_URL /api root. 204 No Content returns
// `undefined as T` so DELETE-style endpoints don't trip over an empty
// body.
export async function apiRequest<T>(
  fullPath: string,
  init?: RequestInit,
  isRetry = false,
): Promise<T> {
  // Workspace pane: literal /api/... paths (native board, dispatcher, bots, …)
  // bypass BASE_URL, so scope them here — the single choke point every
  // apiRequest caller flows through. Paths already carrying the scope (built
  // from BASE_URL = apiBase(), i.e. "/x/<id>/api/…") don't start with "/api",
  // so they're never double-prefixed. No-op outside a pane (scopePrefix() "").
  if (fullPath.startsWith("/api")) fullPath = scopePrefix() + fullPath;
  const res = await fetch(fullPath, {
    credentials: "include",
    headers: { "Content-Type": "application/json", ...(init?.headers ?? {}) },
    ...init,
  });
  // 401 → the access JWT likely just expired. Try ONE silent refresh
  // (POST /auth/refresh, rotating the long-lived refresh token) and replay
  // the request; only sign out if the refresh itself fails. Without this the
  // first call after the 15-minute access-token TTL bounces the user to
  // login even though their refresh token is valid for weeks. Skip the retry
  // for the refresh endpoint itself (avoid a loop) and when already retried.
  // In a workspace pane the desktop owns the token lifecycle: the demux proxy
  // strips the request Cookie and injects a Bearer (refreshed off the OS-held
  // jar by the cloudRoundTripper + background loop), so a pane-side
  // POST /auth/refresh has no refresh cookie and can only fail. Skip it — a 401
  // reaching the pane means the desktop's refresh already failed, and recovery
  // is driven from the shell (cloud:auth-expired → re-login → reload). We still
  // fire onUnauthorized so the pane's AuthGate can surface the expired state.
  if (res.status === 401 && !isRetry && !isScopedPane() && !fullPath.endsWith("/auth/refresh")) {
    if (await tryRefreshSession()) {
      return apiRequest<T>(fullPath, init, true);
    }
    if (onUnauthorized) onUnauthorized();
  } else if (res.status === 401 && onUnauthorized) {
    onUnauthorized();
  }
  if (!res.ok) {
    const detail = await extractErrorDetail(res);
    throw new ApiError(
      res.status,
      `API error ${res.status}: ${detail.message}`,
      detail.errorCode,
    );
  }
  // 204 No Content (e.g. DELETE endpoints) has an empty body. Don't
  // try to parse it — return undefined and let the typed caller cast.
  if (res.status === 204) return undefined as unknown as T;
  // A 2xx with a non-JSON body — e.g. the SPA index.html served for an
  // unrouted /api path — would make res.json() throw a cryptic
  // "JSON.parse: unexpected character at line 1 column 1". Surface a clear,
  // actionable error instead. (The backend now returns a JSON 404 for such
  // paths; this is defense-in-depth against any other non-JSON 2xx.)
  try {
    return (await res.json()) as T;
  } catch {
    const ct = res.headers.get("content-type") ?? "unknown";
    throw new ApiError(
      res.status,
      `expected JSON from ${fullPath} but received ${ct} — the endpoint may be unavailable in this mode`,
    );
  }
}

interface ExtractedErrorDetail {
  message: string;
  errorCode?: string;
}

// extractErrorDetail decodes both the human-facing message and the optional
// stable machine code from a JSON error envelope. It consumes the response
// body once so apiRequest can preserve both fields on ApiError.
async function extractErrorDetail(res: Response): Promise<ExtractedErrorDetail> {
  const text = await res.text();
  if (!text) return { message: res.statusText || "" };
  try {
    const body = JSON.parse(text) as unknown;
    if (body && typeof body === "object") {
      const env = body as {
        error?: unknown;
        error_code?: unknown;
        message?: unknown;
        detail?: unknown;
        reset_at?: unknown;
      };
      const errorCode =
        typeof env.error_code === "string" && env.error_code
          ? env.error_code
          : undefined;
      // Quota/launch denial envelopes carry the human-readable copy in
      // `detail`; prefer it (with the short `error` token as a prefix)
      // so toasts read "monthly_run_quota_exceeded: 1000/1000 — resets …".
      if (typeof env.detail === "string" && env.detail) {
        const token = typeof env.error === "string" && env.error ? env.error : "";
        const tail = typeof env.reset_at === "string" ? ` (resets ${env.reset_at})` : "";
        return {
          message: token ? `${token}: ${env.detail}${tail}` : `${env.detail}${tail}`,
          errorCode,
        };
      }
      if (typeof env.error === "string" && env.error) {
        return { message: env.error, errorCode };
      }
      if (typeof env.message === "string" && env.message) {
        return { message: env.message, errorCode };
      }
    }
  } catch {
    // Not JSON — fall through to the raw text.
  }
  return { message: text };
}

// extractErrorMessage prefers a structured envelope field (`error` or
// `message`) over the raw body, so the toast shown to the user reads
// "forbidden" rather than `{"error":"forbidden"}` for the common Go
// `httpError` shape served by pkg/server. Exported so other api/*.ts
// modules that hit `fetch` directly (file blobs, backend detect, …)
// share the same error-shape rendering.
export async function extractErrorMessage(res: Response): Promise<string> {
  return (await extractErrorDetail(res)).message;
}

/**
 * Wire-format diagnostic mirror of `ir.DiagnosticDTO` on the Go side.
 * Code/NodeID/EdgeID/Hint may be empty for parser-only paths.
 */
export interface DiagnosticIssue {
  code?: string;
  severity: "error" | "warning";
  message: string;
  node_id?: string;
  edge_id?: string;
  hint?: string;
}

export async function parseSource(
  source: string,
): Promise<{ document: IterDocument; diagnostics: string[]; issues?: DiagnosticIssue[] }> {
  return request("/parse", {
    method: "POST",
    body: JSON.stringify({ source }),
  });
}

export async function unparse(document: IterDocument): Promise<string> {
  const res = await request<{ source: string }>("/unparse", {
    method: "POST",
    body: JSON.stringify({ document }),
  });
  return res.source;
}

export async function validate(
  document: IterDocument,
  signal?: AbortSignal,
): Promise<{
  diagnostics: string[];
  warnings: string[];
  issues?: DiagnosticIssue[];
}> {
  return request("/validate", {
    method: "POST",
    body: JSON.stringify({ document }),
    signal,
  });
}

export interface ExampleEntry {
  /** Relative load name, e.g. "whats-next/main.bot". The technical bot
   *  id is its first path segment ("whats-next"). */
  name: string;
  /** Bundle persona (manifest display_name), e.g. "Nexie". Empty for
   *  embedded recipes that ship no on-disk manifest. */
  display_name?: string;
  /** One-line manifest description, condensed server-side. */
  description?: string;
}

// The /examples endpoint returns the first-class bots as
// {name, display_name} objects (the Home shows the persona).
export async function listExampleEntries(): Promise<ExampleEntry[]> {
  return request("/examples");
}

// listExamples keeps the legacy string[] shape (relative names) for the
// FilePicker + CanvasEmpty surfaces that don't render the persona.
export async function listExamples(): Promise<string[]> {
  return (await listExampleEntries()).map((e) => e.name);
}

export async function loadExample(
  name: string,
): Promise<{ source: string; document: IterDocument; diagnostics: string[] }> {
  // Encode each path segment but keep the slashes so subdirectory
  // examples (e.g. "feature_dev/main.bot") route correctly.
  const encoded = name.split("/").map(encodeURIComponent).join("/");
  return request(`/examples/${encoded}`);
}

// File management

export async function listFiles(): Promise<FileEntry[]> {
  const res = await request<ListFilesResponse>("/files");
  return res.files;
}

// BOTSOURCE_SCHEME marks a virtual editor path that resolves to a team-authored
// bot bundle in the cloud store (pkg/botsource) instead of a local file. It lets
// the whole editor UI stay filesystem-shaped in cloud mode: a tab's file is
// "botsource://<teamId>/<slug>/<relpath>", and openFile/saveFile below route it
// to /api/teams/{id}/bot-sources/{slug} transparently.
export const BOTSOURCE_SCHEME = "botsource://";

export function botSourceEditorPath(teamID: string, slug: string, rel = "main.bot"): string {
  return `${BOTSOURCE_SCHEME}${teamID}/${slug}/${rel}`;
}

// inferCatalogBotId derives a catalog bot id from a workflow file path, mirroring
// the server's inferCatalogBotID: "bots/<id>/main.bot", ".../examples/<id>/main.bot",
// "<id>/main.bot" and "<id>.bot" all map to <id>. Used to fork the bot the editor
// currently shows read-only.
export function inferCatalogBotId(path: string): string {
  const parts = path.split("/").filter(Boolean);
  const i = parts.findIndex((p) => p === "bots" || p === "examples");
  if (i >= 0 && parts[i + 1]) return parts[i + 1]!;
  const last = parts[parts.length - 1] ?? "";
  if (parts.length >= 2 && last === "main.bot") {
    return parts[parts.length - 2] ?? "";
  }
  return last.replace(/\.bot$/, "");
}

export function parseBotSourceEditorPath(
  path: string,
): { teamID: string; slug: string; rel: string } | null {
  if (!path.startsWith(BOTSOURCE_SCHEME)) return null;
  const rest = path.slice(BOTSOURCE_SCHEME.length);
  const parts = rest.split("/");
  if (parts.length < 3 || !parts[0] || !parts[1]) return null;
  return { teamID: parts[0], slug: parts[1], rel: parts.slice(2).join("/") };
}

interface BotSourceFilesResponse {
  id: string;
  slug: string;
  files: Record<string, string>;
  origin?: string;
  version: number;
}

export async function openFile(
  path: string,
): Promise<{ source: string; document: IterDocument; diagnostics: string[]; path: string }> {
  const bs = parseBotSourceEditorPath(path);
  if (bs) {
    const bundle = await apiRequest<BotSourceFilesResponse>(
      `/api/teams/${encodeURIComponent(bs.teamID)}/bot-sources/${encodeURIComponent(bs.slug)}`,
    );
    const source = bundle.files?.[bs.rel] ?? "";
    const parsed = await parseSource(source);
    return { source, document: parsed.document, diagnostics: parsed.diagnostics, path };
  }
  return request("/files/open", {
    method: "POST",
    body: JSON.stringify({ path }),
  });
}

export async function saveFile(
  path: string,
  document: IterDocument,
): Promise<SaveFileResponse> {
  const bs = parseBotSourceEditorPath(path);
  if (bs) {
    const source = await unparse(document);
    // Carry the botsource CAS token. The old per-file editor write omitted it,
    // so two tabs could silently overwrite one another even though the store
    // already supported optimistic concurrency.
    const current = await apiRequest<BotSourceFilesResponse>(
      `/api/teams/${encodeURIComponent(bs.teamID)}/bot-sources/${encodeURIComponent(bs.slug)}`,
    );
    await apiRequest(
      `/api/teams/${encodeURIComponent(bs.teamID)}/bot-sources/${encodeURIComponent(bs.slug)}/files/${bs.rel}`,
      { method: "PUT", body: JSON.stringify({ content: source, version: current.version }) },
    );
    return { path, source };
  }
  return request("/files/save", {
    method: "POST",
    body: JSON.stringify({ path, document }),
  });
}

// Reasoning effort capabilities

export interface EffortCapabilities {
  supported: string[] | null;
  default: string;
  source: "claw-registry" | "codex-cli" | "codex-fallback";
}

export async function fetchEffortCapabilities(
  backend: string,
  model: string,
  signal?: AbortSignal,
): Promise<EffortCapabilities> {
  const params = new URLSearchParams({ backend, model });
  return request<EffortCapabilities>(`/effort-capabilities?${params.toString()}`, { signal });
}

// fetchResolvedEffort asks the server to env-substitute and validate
// a reasoning_effort literal (e.g. "${VIBE_EFFORT:-max}"). Returns the
// resolved enum value, or "" when the literal is empty / expansion
// produced something not in low/medium/high/xhigh/max.
export async function fetchResolvedEffort(
  literal: string,
  signal?: AbortSignal,
): Promise<string> {
  const params = new URLSearchParams({ literal });
  const r = await request<{ resolved: string }>(
    `/resolve-effort?${params.toString()}`,
    { signal },
  );
  return r.resolved;
}

// fetchResolvedModel asks the server to env-substitute a model literal
// (e.g. "${CODEX_MODEL:-openai-codex/gpt-5.6-sol}"). Returns the
// resolved spec, or "" when the literal is empty / expansion produced
// something that does not look like a model (so the canvas can fall
// back to the authored default instead of leaking process env).
export async function fetchResolvedModel(
  literal: string,
  signal?: AbortSignal,
): Promise<string> {
  const params = new URLSearchParams({ literal });
  const r = await request<{ resolved: string }>(
    `/resolve-model?${params.toString()}`,
    { signal },
  );
  return r.resolved;
}
