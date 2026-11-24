// Extracted from api/runs.ts to keep that file focused.
// HTTP plumbing for the run-console client: BASE_URL, request wrapper,
// apiURL, and the cross-store override helpers shared by every submodule.

import { apiRequest, extractErrorMessage } from "../client";
import { withExpectedBirth } from "@/lib/lostRuns";
import { apiBase } from "@/lib/scope";
import { storeOverrideFor } from "@/lib/storeOverride";

export const BASE_URL = apiBase();

// Delegate to the shared apiRequest so this module picks up:
//   - credentials: "include" (needed in cross-origin cloud deployments)
//   - the 401 → onUnauthorized hook the AuthProvider registers
//   - the run's expected birth
// Without this, every run-console call (list, get, launch, cancel,
// resume, ...) silently broke in cloud mode and failed to surface
// expired-session 401s.
export function request<T>(path: string, init?: RequestInit): Promise<T> {
  return apiRequest<T>(`${BASE_URL}${path}`, init);
}

// apiURL returns the full URL for a given API path, suitable for
// hand-off to <a href> / browser download (no JSON parsing). Same
// BASE_URL resolution as `request` so dev / prod / cloud mode all
// work without callers knowing the deployment. A path addressing a run
// whose birth the page knows carries expected_birth: the browser loads
// these URLs by itself, so the query is the only way the server sees which
// run the page meant (@/lib/lostRuns).
export function apiURL(path: string): string {
  return withExpectedBirth(`${BASE_URL}${path}`);
}

// The page's cross-store override (`?store=`), defined in
// @/lib/storeOverride and re-exported for the run-console modules.
export { isSafeStoreParam, readStoreOverrideFromURL, storeOverrideFor } from "@/lib/storeOverride";

// withStoreParam appends `store=<override>` to the given URLSearchParams
// when runId is the page's run and the page shows it from a foreign store.
// No-op otherwise. The routes that read a foreign store call it (the
// snapshot, its events, its workflow, its session board); every other route
// reads the daemon's own store. Throws InvalidStoreOverrideError for an
// override that is not a store path.
export function withStoreParam(qs: URLSearchParams, runId: string): URLSearchParams {
  const override = storeOverrideFor(runId);
  if (override) qs.set("store", override);
  return qs;
}

// Re-export extractErrorMessage so submodules using direct `fetch()`
// calls (artifact downloads, tool-blob streaming) can keep importing
// from a single in-runs location.
export { extractErrorMessage };
