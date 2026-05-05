import type { IterDocument, FileEntry, ListFilesResponse, SaveFileResponse } from "./types";

const BASE_URL = import.meta.env.VITE_API_URL ?? "/api";

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`${BASE_URL}${path}`, {
    headers: { "Content-Type": "application/json" },
    ...init,
  });
  if (!res.ok) {
    throw new Error(`API error ${res.status}: ${await res.text()}`);
  }
  return res.json() as Promise<T>;
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
  return request(`/examples/${encodeURIComponent(name)}`);
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
