import { errorMessage } from "@/lib/errorHints";
import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";

import { listRuns, type RunStatus, type RunSummary } from "@/api/runs";

// Stable empty fallback so the undefined→loaded transition doesn't hand
// the (many) downstream useMemos a fresh [] reference each render.
const EMPTY_RUNS: RunSummary[] = [];

const POLL_INTERVAL_FAST_MS = 3000;
const POLL_INTERVAL_SLOW_MS = 8000;
// Nothing queued / running / awaiting the operator: the list can only
// change through an external launch, so poll lazily (focus refetch
// still snaps the list fresh when the operator returns to the tab).
const POLL_INTERVAL_IDLE_MS = 15000;
// Above this many queued runs we slow polling to relieve the cloud
// server. Mirrors RunListView's contract — see cloud-ready plan §F.
const QUEUED_BACKOFF_THRESHOLD = 10;

const ACTIVE_STATUSES: RunStatus[] = ["queued", "running", "paused_waiting_human"];

export function computePollingInterval(
  counts: Partial<Record<RunStatus, number>>,
): number {
  const queued = counts.queued ?? 0;
  if (queued >= QUEUED_BACKOFF_THRESHOLD) return POLL_INTERVAL_SLOW_MS;
  const active = ACTIVE_STATUSES.reduce((n, s) => n + (counts[s] ?? 0), 0);
  return active > 0 ? POLL_INTERVAL_FAST_MS : POLL_INTERVAL_IDLE_MS;
}

export interface UseRunsOptions {
  status?: RunStatus | "";
  limit?: number;
  // Repo scopes the list to a stable forge slug (project_path) — cloud
  // mode only (server-side, index-backed). Local-mode folder filtering
  // is client-side, so callers leave this empty in local mode.
  repo?: string;
  // When false, the hook skips fetching and returns the empty result.
  // Used by surfaces that only need the runs list while a UI is open
  // (e.g. the global command palette) to avoid background polling.
  enabled?: boolean;
}

export interface UseRunsResult {
  runs: RunSummary[];
  counts: Partial<Record<RunStatus, number>>;
  loading: boolean;
  error: string | null;
}

// Polls the runs list at an adaptive interval (3s while runs are active,
// 8s when the queue is deep, 15s when everything is terminal or the list
// is empty). TanStack Query handles tab visibility natively
// (`refetchIntervalInBackground: false` pauses polling while the tab
// is hidden) and de-dupes consumers that mount the same key, so the
// previous fingerprint + visibilitychange machinery falls away.
export function useRuns(opts: UseRunsOptions = {}): UseRunsResult {
  const { status = "", limit, repo = "", enabled = true } = opts;
  const query = useQuery<RunSummary[]>({
    queryKey: ["runs", status, limit, repo],
    queryFn: () =>
      listRuns({ status: status || undefined, limit, repo: repo || undefined }),
    enabled,
    refetchInterval: (q) => {
      const data = q.state.data;
      if (!data) return POLL_INTERVAL_FAST_MS;
      const counts: Partial<Record<RunStatus, number>> = {};
      for (const r of data) counts[r.status] = (counts[r.status] ?? 0) + 1;
      return computePollingInterval(counts);
    },
    refetchIntervalInBackground: false,
  });

  const runs = query.data ?? EMPTY_RUNS;
  const counts = useMemo(() => {
    const m: Partial<Record<RunStatus, number>> = {};
    for (const r of runs) m[r.status] = (m[r.status] ?? 0) + 1;
    return m;
  }, [runs]);

  return {
    runs,
    counts,
    loading: query.isLoading,
    error: query.error ? errorMessage(query.error) : null,
  };
}
