import { useCallback, useEffect, useRef } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";

import { listRunFiles, type RunFiles, type RunFilesMode } from "@/api/runs";
import { useRunStore } from "@/store/run";

// useRunFiles fetches the modified-files listing for runId and auto-
// refetches whenever an event suggests the working tree just changed
// (node_finished, run_finished, run_failed, run_cancelled, run_paused).
// A single fetch is debounced over a 300ms window to coalesce bursts —
// fan-out branches finishing back-to-back would otherwise trigger a
// flurry of identical requests.
//
// `mode` selects the view (uncommitted vs branch range). Changing the
// mode triggers an immediate refetch — it's part of the cache key so
// the previous mode's payload doesn't leak through.
//
// Callers also receive a `refresh()` callback for explicit reload after
// the user clicks the "refresh" button in the panel.
const REFRESH_EVENTS = new Set([
  "node_finished",
  "run_finished",
  "run_failed",
  "run_cancelled",
  "run_paused",
]);

const DEBOUNCE_MS = 300;

export function useRunFiles(
  runId: string | null,
  mode: RunFilesMode = "",
): {
  data: RunFiles | null;
  loading: boolean;
  error: string | null;
  refresh: () => void;
} {
  const queryClient = useQueryClient();
  const query = useQuery<RunFiles>({
    queryKey: ["run-files", runId, mode],
    queryFn: () => listRunFiles(runId!, { mode }),
    enabled: !!runId,
  });

  const refresh = useCallback(() => {
    if (!runId) return;
    queryClient.invalidateQueries({ queryKey: ["run-files", runId, mode] });
  }, [queryClient, runId, mode]);

  // High-water mark on event seq so we only react to new events.
  const lastSeenSeqRef = useRef<number>(-1);
  const debounceRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  // Reset the seq tracker when the run id changes.
  useEffect(() => {
    lastSeenSeqRef.current = -1;
  }, [runId]);

  const events = useRunStore((s) => s.events);

  useEffect(() => {
    if (!runId || events.length === 0) return;
    let triggered = false;
    for (const e of events) {
      if (e.seq <= lastSeenSeqRef.current) continue;
      if (REFRESH_EVENTS.has(e.type)) {
        triggered = true;
      }
    }
    // Always advance the marker so we don't re-process old events even
    // when the latest batch contains nothing relevant.
    const tail = events[events.length - 1];
    if (tail) lastSeenSeqRef.current = tail.seq;
    if (!triggered) return;
    if (debounceRef.current) clearTimeout(debounceRef.current);
    debounceRef.current = setTimeout(() => {
      refresh();
    }, DEBOUNCE_MS);
    return () => {
      if (debounceRef.current) {
        clearTimeout(debounceRef.current);
        debounceRef.current = null;
      }
    };
  }, [events, runId, refresh]);

  return {
    data: query.data ?? null,
    loading: query.isLoading,
    error: query.error ? (query.error as Error).message : null,
    refresh,
  };
}
