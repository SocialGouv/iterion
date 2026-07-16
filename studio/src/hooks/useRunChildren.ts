import { errorMessage } from "@/lib/errorHints";
import { useQuery } from "@tanstack/react-query";

import { getRunChildren, type RunSummary } from "@/api/runs";
import { isSettledRunStatus } from "@/lib/subRuns";

// Optional polling mode for callers that need to notice children as
// they appear (the run console's sub-run tabs — subbot children are
// spawned mid-run, so a single fetch on mount misses them). When
// refetchIntervalMs is set, the query re-fetches on that cadence while
// the parent is still active OR any known child is unsettled, and
// stops once everything settled (nothing new can appear then).
export interface UseRunChildrenOptions {
  refetchIntervalMs?: number;
  // Caller-supplied "the parent run is still producing work" bit (the
  // hook only sees the children, not the parent's status).
  parentActive?: boolean;
}

// useRunChildren fetches a run's shard/child subtree (T4b, refs #125) —
// the runs whose parent_run_id points at runId. Lazy by design: the
// query only fires when `enabled` is true (default), so a caller that
// knows a run has no children (or wants to defer until expand) can gate
// the fetch and avoid an N+1 across a run list. Mirrors useRunCommits'
// return shape, minus the event-driven refresh — single-fetch by
// default; pass options.refetchIntervalMs to poll (see above).
export function useRunChildren(
  runId: string | null,
  enabled = true,
  options?: UseRunChildrenOptions,
): {
  data: RunSummary[];
  loading: boolean;
  error: string | null;
} {
  const { refetchIntervalMs, parentActive } = options ?? {};
  const query = useQuery<RunSummary[]>({
    queryKey: ["run-children", runId],
    queryFn: () => getRunChildren(runId!),
    enabled: !!runId && enabled,
    refetchInterval: refetchIntervalMs
      ? (q) => {
          if (parentActive) return refetchIntervalMs;
          const children = q.state.data;
          if (children?.some((c) => !isSettledRunStatus(c.status))) {
            return refetchIntervalMs;
          }
          return false;
        }
      : undefined,
  });

  return {
    data: query.data ?? [],
    loading: query.isLoading,
    error: query.error ? errorMessage(query.error) : null,
  };
}
