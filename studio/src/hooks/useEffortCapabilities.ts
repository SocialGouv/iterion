import { useQuery, useQueryClient, type QueryClient } from "@tanstack/react-query";

import {
  fetchEffortCapabilities,
  type EffortCapabilities,
} from "@/api/client";

// effortBackendKey routes an unset backend through "claw" — that's
// where iterion sends LLM calls when the workflow doesn't pick
// claude_code or codex. Used as the backend half of the cache key.
export function effortBackendKey(backend: string | undefined): string {
  return backend?.trim() || "claw";
}

function effortQueryKey(backend: string, model: string) {
  return ["effort-capabilities", backend, model] as const;
}

// Capabilities never change during a session (claw-code-go's registry
// is built-in; codex has its own server-side cache), so the entries are
// kept indefinitely.
const STALE_FOREVER = Number.POSITIVE_INFINITY;

/**
 * Synchronous lookup against the React Query cache. Returns `undefined`
 * when the (backend, model) pair has not been fetched yet. Used by
 * RunCanvasIR for first-render seeding before the async fetch lands.
 */
export function getCachedEffortCapabilities(
  queryClient: QueryClient,
  backend: string,
  model: string,
): EffortCapabilities | undefined {
  return queryClient.getQueryData<EffortCapabilities>(
    effortQueryKey(backend, model),
  );
}

/**
 * Deduplicated promise resolver backed by React Query. Multiple
 * callers awaiting the same (backend, model) share the same in-flight
 * request; the result is cached forever for the session.
 */
export function fetchAndCacheEffortCapabilities(
  queryClient: QueryClient,
  backend: string,
  model: string,
): Promise<EffortCapabilities> {
  return queryClient.fetchQuery({
    queryKey: effortQueryKey(backend, model),
    queryFn: () => fetchEffortCapabilities(backend, model),
    staleTime: STALE_FOREVER,
    gcTime: STALE_FOREVER,
  });
}

export interface UseEffortCapabilitiesResult {
  // null while the first fetch is pending.
  capabilities: EffortCapabilities | null;
  loading: boolean;
  error: string | null;
}

// useEffortCapabilities returns the supported reasoning_effort levels
// for the given (backend, model) pair, fetched lazily and cached for
// the session.
export function useEffortCapabilities(
  backend: string | undefined,
  model: string | undefined,
): UseEffortCapabilitiesResult {
  const enabled = !!backend && !!model;
  const query = useQuery<EffortCapabilities>({
    // When disabled, useQuery still requires a key — pick a stable one.
    queryKey: enabled
      ? effortQueryKey(backend!, model!)
      : ["effort-capabilities", "__disabled__"],
    queryFn: () => fetchEffortCapabilities(backend!, model!),
    enabled,
    staleTime: STALE_FOREVER,
    gcTime: STALE_FOREVER,
  });
  return {
    capabilities: query.data ?? null,
    loading: enabled && query.isLoading,
    error: query.error ? (query.error as Error).message : null,
  };
}

// React-side convenience: bind the imperative helpers to the
// component's query client. Use when a useEffect needs to do a sync
// cache lookup or fire a one-off fetch without subscribing.
export function useEffortCapabilitiesClient() {
  const queryClient = useQueryClient();
  return {
    getCached: (backend: string, model: string) =>
      getCachedEffortCapabilities(queryClient, backend, model),
    fetch: (backend: string, model: string) =>
      fetchAndCacheEffortCapabilities(queryClient, backend, model),
  };
}
