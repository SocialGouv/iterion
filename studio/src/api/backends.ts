// Mirrors pkg/backend/detect.Report. Keep the field names in sync — the
// Go handler returns json:"snake_case" and we deserialise verbatim.

import { request } from "./client";

export interface BackendStatus {
  name: "claude_code" | "codex" | "claw";
  available: boolean;
  auth: "oauth" | "api_key" | "none";
  // Go serialises nil slices as `null`, so the field may be missing or null.
  sources?: string[] | null;
  hints?: string[] | null;
}

export interface ProviderStatus {
  name: "anthropic" | "openai" | "xai" | "foundry" | "bedrock" | "vertex" | "zai";
  available: boolean;
  source: string;
  suggested_model?: string;
  // OverriddenSources lists detected credentials that exist on the host
  // but won't be used because `source` takes precedence. Each label
  // includes a trailing "(overridden by …)" annotation that the UI
  // detects to render the entry struck-through. Only OpenAI currently
  // populates this (API key vs ChatGPT-OAuth) but the shape is generic.
  overridden_sources?: string[] | null;
}

export interface BackendDetectReport {
  preference_order: string[];
  resolved_default: string;
  backends: BackendStatus[];
  providers: ProviderStatus[];
}

export async function fetchBackendDetect(
  opts: { signal?: AbortSignal; force?: boolean } = {},
): Promise<BackendDetectReport> {
  // Route through the shared `request` wrapper so this call gets the same
  // silent 401 → /auth/refresh → replay handling every other /api/* call has.
  // Without it, the LLM-credentials panel was the one place that surfaced a
  // raw "401 authentication required" once the short-lived access cookie
  // expired, even though the refresh token was still valid (the browser drops
  // the access cookie at its TTL, so requireAuth sees no bearer at all).
  //
  // Cache-bust both the server-side TTL cache (?force=1) and any browser /
  // webview HTTP cache (cache: "no-store" + Cache-Control header). The Wails
  // webview is particularly aggressive about caching identical GETs.
  const path = opts.force ? "/backends/detect?force=1" : "/backends/detect";
  return request<BackendDetectReport>(path, {
    method: "GET",
    signal: opts.signal,
    cache: opts.force ? "no-store" : "default",
    headers: opts.force ? { "Cache-Control": "no-cache" } : undefined,
  });
}
