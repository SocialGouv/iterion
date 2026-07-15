import { errorMessage } from "@/lib/errorHints";
import { useQuery } from "@tanstack/react-query";

import { getRunChildren, type RunSummary } from "@/api/runs";

// useRunChildren fetches a run's shard/child subtree (T4b, refs #125) —
// the runs whose parent_run_id points at runId. Lazy by design: the
// query only fires when `enabled` is true (default), so a caller that
// knows a run has no children (or wants to defer until expand) can gate
// the fetch and avoid an N+1 across a run list. Mirrors useRunCommits'
// return shape, minus the event-driven refresh (the child set only
// changes when new shards are spawned, which the run's own event stream
// already reflows).
export function useRunChildren(
  runId: string | null,
  enabled = true,
): {
  data: RunSummary[];
  loading: boolean;
  error: string | null;
} {
  const query = useQuery<RunSummary[]>({
    queryKey: ["run-children", runId],
    queryFn: () => getRunChildren(runId!),
    enabled: !!runId && enabled,
  });

  return {
    data: query.data ?? [],
    loading: query.isLoading,
    error: query.error ? errorMessage(query.error) : null,
  };
}
