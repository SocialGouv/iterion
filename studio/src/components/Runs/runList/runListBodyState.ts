// Pure decision for which body the runs list should render. Extracted so
// the branch order — cold-skeleton → error → empty → no-matches → list —
// can be locked by unit tests without mounting the whole (WS/store-heavy,
// Virtuoso-measured) RunListView under jsdom.

export type RunListBodyState =
  | "skeleton" // cold load, no cached data yet
  | "error" // fetch failed
  | "empty" // scope has zero runs at all
  | "no-matches" // runs exist but filters hide them all
  | "list"; // render the (virtualized) list

export interface RunListBodyInput {
  // useRuns: true only on the very first load of a key (no cached data).
  loading: boolean;
  // useRuns error message, or null.
  error: string | null;
  // total fetched runs for the current scope (pre client-side filter).
  runCount: number;
  // runs surviving the client-side filters.
  filteredCount: number;
}

export function runListBodyState({
  loading,
  error,
  runCount,
  filteredCount,
}: RunListBodyInput): RunListBodyState {
  // Cold load wins: no cached rows to keep on screen, so show the
  // skeleton. A cached refetch (loading=false, runCount>0) falls through
  // to "list" and the caller overlays a refreshing indicator instead —
  // the list never blanks on a scope switch.
  if (loading && runCount === 0) return "skeleton";
  if (error) return "error";
  if (runCount === 0) return "empty";
  if (filteredCount === 0) return "no-matches";
  return "list";
}

// Whether the refreshing overlay should be shown: only when cached rows
// are on screen (never during the cold skeleton or an error/empty state).
export function showRefreshingOverlay(input: {
  refreshing: boolean;
  repoScopeLoading: boolean;
  runCount: number;
}): boolean {
  return (input.refreshing || input.repoScopeLoading) && input.runCount > 0;
}
