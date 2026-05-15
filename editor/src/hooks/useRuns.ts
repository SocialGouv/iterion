import { useEffect, useMemo, useRef, useState } from "react";

import { listRuns, type RunStatus, type RunSummary } from "@/api/runs";

// Cheap fingerprint of a run list: each run reduces to its
// status-affecting fields. If two consecutive polls produce the same
// fingerprint, we skip the setState to avoid re-rendering every
// consumer (HomeView's banner + recent panel re-filter on every poll
// otherwise, even when nothing changed).
function fingerprint(runs: RunSummary[]): string {
  const parts: string[] = [];
  for (const r of runs) {
    parts.push(
      `${r.id}:${r.status}:${r.updated_at}:${r.finished_at ?? ""}:${r.queue_position ?? ""}`,
    );
  }
  return parts.join("|");
}

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
// queue is deep). One hook instance per consumer; siblings on the same
// view should be fed from a shared parent invocation so we don't fan
// out duplicate requests.
export function useRuns(opts: UseRunsOptions = {}): UseRunsResult {
  const { status = "", limit } = opts;
  const [runs, setRuns] = useState<RunSummary[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const lastFingerprint = useRef<string>("");

  const counts = useMemo(() => {
    const m: Partial<Record<RunStatus, number>> = {};
    for (const r of runs) m[r.status] = (m[r.status] ?? 0) + 1;
    return m;
  }, [runs]);

  const pollMs = computePollingInterval(counts);

  useEffect(() => {
    let cancelled = false;
    const fetchRuns = async () => {
      try {
        const out = await listRuns({
          status: status || undefined,
          limit,
        });
        if (cancelled) return;
        const fp = fingerprint(out);
        if (fp !== lastFingerprint.current) {
          lastFingerprint.current = fp;
          setRuns(out);
        }
        setError(null);
        setLoading(false);
      } catch (e) {
        if (!cancelled) {
          setError((e as Error).message);
          setLoading(false);
        }
      }
    };
    fetchRuns();
    const id = setInterval(fetchRuns, pollMs);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, [status, limit, pollMs]);

  return { runs, counts, loading, error };
}
