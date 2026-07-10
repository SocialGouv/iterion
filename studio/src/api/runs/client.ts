// Extracted from api/runs.ts to keep that file focused.
// HTTP plumbing for the run-console client: BASE_URL, request wrapper,
// apiURL, and the cross-store override helpers shared by every submodule.

import { apiRequest, extractErrorMessage } from "../client";
import { apiBase } from "@/lib/scope";

export const BASE_URL = apiBase();

// Delegate to the shared apiRequest so this module picks up:
//   - credentials: "include" (needed in cross-origin cloud deployments)
//   - the 401 → onUnauthorized hook the AuthProvider registers
// Without this, every run-console call (list, get, launch, cancel,
// resume, ...) silently broke in cloud mode and failed to surface
// expired-session 401s.
export function request<T>(path: string, init?: RequestInit): Promise<T> {
  return apiRequest<T>(`${BASE_URL}${path}`, init);
}

// apiURL returns the full URL for a given API path, suitable for
// hand-off to <a href> / browser download (no JSON parsing). Same
// BASE_URL resolution as `request` so dev / prod / cloud mode all
// work without callers knowing the deployment.
export function apiURL(path: string): string {
  return `${BASE_URL}${path}`;
}

// readStoreOverrideFromURL returns the current page's `?store=` query
// param, if any. The Home banner appends it when navigating to a run
// living in a foreign iterion store (typically the global ~/.iterion/
// slot) so this daemon's read endpoints route via the cross-store
// proxy (pkg/server/runs.go::resolveCrossStore).
//
// Empty string when not in a browser context, when no override is set,
// or when the value isn't a non-empty string.
export function readStoreOverrideFromURL(): string {
  if (typeof window === "undefined") return "";
  try {
    const v = new URLSearchParams(window.location.search).get("store");
    return isSafeStoreParam(v) ? (v as string) : "";
  } catch {
    return "";
  }
}

// isSafeStoreParam validates the cross-store override path shape before
// forwarding it to the daemon. The server-side resolveCrossStore guard
// rejects unknown stores, but client-side filtering is defence-in-depth
// against a hostile `?store=...` URL handed to a victim — keep the
// charset tight and reject path-traversal segments.
// Exported so the WS hook can reuse the same predicate.
export function isSafeStoreParam(v: string | null): boolean {
  if (!v || v.length === 0 || v.length > 512) return false;
  if (!/^[A-Za-z0-9_./-]+$/.test(v)) return false;
  if (v.includes("..")) return false;
  return true;
}

// withStoreParam appends `store=<override>` to the given URLSearchParams
// when a cross-store override is active. No-op otherwise. Centralised
// here so every run-scoped API call routes consistently.
export function withStoreParam(qs: URLSearchParams): URLSearchParams {
  const override = readStoreOverrideFromURL();
  if (override) qs.set("store", override);
  return qs;
}

// Re-export extractErrorMessage so submodules using direct `fetch()`
// calls (artifact downloads, tool-blob streaming) can keep importing
// from a single in-runs location.
export { extractErrorMessage };
