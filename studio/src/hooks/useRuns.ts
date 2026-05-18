import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";

import { listRuns, type RunStatus, type RunSummary } from "@/api/runs";

const POLL_INTERVAL_FAST_MS = 3000;
const POLL_INTERVAL_SLOW_MS = 8000;
// Above this many queued runs we slow polling to relieve the cloud
// server. Mirrors RunListView's contract — see cloud-ready plan §F.
const QUEUED_BACKOFF_THRESHOLD = 10;

export function computePollingInterval(
  counts: Partial<Record<RunStatus, number>>,
): number {
  const queued = counts.queued ?? 0;
  return queued >= QUEUED_BACKOFF_THRESHOLD
    ? POLL_INTERVAL_SLOW_MS
    : POLL_INTERVAL_FAST_MS;
}

export interface UseRunsOptions {
  status?: RunStatus | "";
  limit?: number;
}

export interface UseRunsResult {
  runs: RunSummary[];
  counts: Partial<Record<RunStatus, number>>;
  loading: boolean;
  error: string | null;
}

// Polls the runs list at an adaptive interval (3s normally, 8s when the
// queue is deep). TanStack Query handles tab visibility natively
// (`refetchIntervalInBackground: false` pauses polling while the tab
// is hidden) and de-dupes consumers that mount the same key, so the
// previous fingerprint + visibilitychange machinery falls away.
export function useRuns(opts: UseRunsOptions = {}): UseRunsResult {
  const { status = "", limit } = opts;
  const query = useQuery<RunSummary[]>({
    queryKey: ["runs", status, limit],
    queryFn: () => listRuns({ status: status || undefined, limit }),
    refetchInterval: (q) => {
      const data = q.state.data;
      if (!data) return POLL_INTERVAL_FAST_MS;
      let queued = 0;
      for (const r of data) if (r.status === "queued") queued++;
      return queued >= QUEUED_BACKOFF_THRESHOLD
        ? POLL_INTERVAL_SLOW_MS
        : POLL_INTERVAL_FAST_MS;
    },
    refetchIntervalInBackground: false,
  });

  const runs = query.data ?? [];
  const counts = useMemo(() => {
    const m: Partial<Record<RunStatus, number>> = {};
    for (const r of runs) m[r.status] = (m[r.status] ?? 0) + 1;
    return m;
  }, [runs]);

  return {
    runs,
    counts,
    loading: query.isLoading,
    error: query.error ? (query.error as Error).message : null,
  };
}
