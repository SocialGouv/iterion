import { useQuery } from "@tanstack/react-query";

import { fetchModelCapabilities, type ModelCapabilities } from "@/api/client";
import { useDebounce } from "@/hooks/useDebounce";

// A curated answer can still improve, so it is refetched; an aggregator one
// cannot, so it is kept for the session.
//
// This is deliberately NOT useEffortCapabilities' flat "cache forever".
// Resolution is non-blocking by design: a cold lookup returns the curated
// value and only STARTS the background spec refresh, installing the fetched
// table moments later. Pinning that first price-less answer for the session
// would mean the tooltip never shows a price on a cold start — which is the
// whole feature, and exactly the case a developer with a warm cache would
// never see.
const CURATED_STALE_MS = 30_000;
const AGGREGATOR_STALE_MS = Number.POSITIVE_INFINITY;

// Every picker feeding this hook is a free-text <Input>, so the spec changes
// once per keystroke and each distinct value is its own query key. Settle it
// before it reaches the key: without this, typing "anthropic/claude-opus-5"
// issues one request per character. The initial value is NOT delayed
// (useDebounce seeds its state with it), so the common case — a picker
// mounting on an already-selected model — still resolves immediately.
const SPEC_DEBOUNCE_MS = 150;

// Inactive entries are evicted after five minutes. `staleTime: Infinity` is
// what pins a settled aggregator answer for the session; `gcTime` only decides
// how long an UNMOUNTED key is retained, and the keys this hook produces are
// mostly the half-typed prefixes debouncing did not absorb. Keeping those
// forever grows the cache for the life of the page with entries nothing will
// ever read again — five minutes still covers a picker closed and reopened.
const GC_MS = 5 * 60_000;

/**
 * modelCapsStaleTime is the caching rule, exported so it can be asserted
 * directly. Through the hook it is only observable by waiting out a 30-second
 * window, and a test that forced a refetch instead would pass just as happily
 * against "cache forever" — the bug this rule exists to prevent.
 */
export function modelCapsStaleTime(
  source: ModelCapabilities["source"] | undefined,
): number {
  return source === "aggregator" ? AGGREGATOR_STALE_MS : CURATED_STALE_MS;
}

export interface UseModelCapabilitiesResult {
  // null until the first response lands, and whenever no model is selected.
  capabilities: ModelCapabilities | null;
  loading: boolean;
}

/**
 * useModelCapabilities resolves a model's limits and published price.
 *
 * `spec` may be a qualified "provider/model-id" or a bare model id — the
 * server serves both. An unresolved env literal (`${VAR}`) is skipped: expand
 * it first (see effectiveModel), or the query would key on a template string
 * no aggregator has ever heard of.
 */
export function useModelCapabilities(
  spec: string | undefined,
): UseModelCapabilitiesResult {
  const trimmed = useDebounce(spec?.trim() ?? "", SPEC_DEBOUNCE_MS);
  const enabled = trimmed !== "" && !trimmed.includes("$");

  const query = useQuery<ModelCapabilities>({
    queryKey: ["model-capabilities", trimmed],
    queryFn: ({ signal }) => fetchModelCapabilities(trimmed, signal),
    enabled,
    staleTime: (q) => modelCapsStaleTime(q.state.data?.source),
    gcTime: GC_MS,
    refetchOnMount: true,
    // A capability caption is decoration: a failed lookup must leave the
    // picker usable, so nothing here retries or surfaces an error.
    retry: false,
  });

  return {
    capabilities: query.data ?? null,
    loading: enabled && query.isLoading,
  };
}
