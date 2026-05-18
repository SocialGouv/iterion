import type { IterDocument, FileEntry, ListFilesResponse, SaveFileResponse } from "./types";

const BASE_URL = import.meta.env.VITE_API_URL ?? "/api";

// onUnauthorized fires when the studio server returns 401 on any
// /api/* call. The AuthProvider registers a handler that flips its
// state to `anonymous` so the App swaps in the Login view.
let onUnauthorized: (() => void) | null = null;

export function setUnauthorizedHandler(fn: (() => void) | null) {
  onUnauthorized = fn;
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
export async function apiRequest<T>(fullPath: string, init?: RequestInit): Promise<T> {
  const res = await fetch(fullPath, {
    credentials: "include",
    headers: { "Content-Type": "application/json", ...(init?.headers ?? {}) },
    ...init,
  });
  if (res.status === 401 && onUnauthorized) {
    onUnauthorized();
  }
  if (!res.ok) {
    throw new Error(`API error ${res.status}: ${await extractErrorMessage(res)}`);
  }
  // 204 No Content (e.g. DELETE endpoints) has an empty body. Don't
  // try to parse it — return undefined and let the typed caller cast.
  if (res.status === 204) return undefined as unknown as T;
  return res.json() as Promise<T>;
}

// extractErrorMessage prefers a structured envelope field (`error` or
// `message`) over the raw body, so the toast shown to the user reads
// "forbidden" rather than `{"error":"forbidden"}` for the common Go
// `httpError` shape served by pkg/server.
async function extractErrorMessage(res: Response): Promise<string> {
  const text = await res.text();
  if (!text) return res.statusText || "";
  try {
    const body = JSON.parse(text) as unknown;
    if (body && typeof body === "object") {
      const env = body as { error?: unknown; message?: unknown };
      if (typeof env.error === "string" && env.error) return env.error;
      if (typeof env.message === "string" && env.message) return env.message;
    }
  } catch {
    // Not JSON — fall through to the raw text.
  }
  return text;
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

export async function listExamples(): Promise<string[]> {
  return request("/examples");
}

export async function loadExample(
  name: string,
): Promise<{ source: string; document: IterDocument; diagnostics: string[] }> {
  // Encode each path segment but keep the slashes so subdirectory
  // examples (e.g. "bots/vibe_feature_dev.bot") route correctly.
  const encoded = name.split("/").map(encodeURIComponent).join("/");
  return request(`/examples/${encoded}`);
}

// File management

export async function listFiles(): Promise<FileEntry[]> {
  const res = await request<ListFilesResponse>("/files");
  return res.files;
}

export async function openFile(
  path: string,
): Promise<{ source: string; document: IterDocument; diagnostics: string[]; path: string }> {
  return request("/files/open", {
    method: "POST",
    body: JSON.stringify({ path }),
  });
}

export async function saveFile(
  path: string,
  document: IterDocument,
): Promise<SaveFileResponse> {
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
