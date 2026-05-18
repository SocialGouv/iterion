import { useQuery } from "@tanstack/react-query";

import { listGlobalActiveRuns, type GlobalActiveRun } from "@/api/runs";

const POLL_INTERVAL_MS = 8000;

export interface UseGlobalActiveRunsResult {
  runs: GlobalActiveRun[];
  loading: boolean;
  error: string | null;
}

// Polls /api/runs/global-active so the Home view can surface runs
// active in OTHER iterion stores (other projects, the no-project
// ~/.iterion slot). Slower poll than useRuns because the inputs are
// runs the user is NOT currently watching closely; 8s keeps the
// indicator fresh without driving filesystem walks every 3s.
export function useGlobalActiveRuns(): UseGlobalActiveRunsResult {
  const query = useQuery<GlobalActiveRun[]>({
    queryKey: ["global-active-runs"],
    queryFn: listGlobalActiveRuns,
    refetchInterval: POLL_INTERVAL_MS,
    refetchIntervalInBackground: false,
  });
  return {
    runs: query.data ?? [],
    loading: query.isLoading,
    error: query.error ? (query.error as Error).message : null,
  };
}
