import { useCallback, useEffect, useRef } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";

import { listRunCommits, type RunCommits } from "@/api/runs";
import { useRunStore } from "@/store/run";

// Mirror of useRunFiles for the Commits tab. Same debounce + race
// guards; the trigger set is identical because workflow commits are
// produced inside `git commit` tool calls — they fire as part of a
// node_finished event, not via a dedicated commit event.
const REFRESH_EVENTS = new Set([
  "node_finished",
  "run_finished",
  "run_failed",
  "run_cancelled",
  "run_paused",
]);

const DEBOUNCE_MS = 300;

export function useRunCommits(runId: string | null): {
  data: RunCommits | null;
  loading: boolean;
  error: string | null;
  refresh: () => void;
} {
  const queryClient = useQueryClient();
  const query = useQuery<RunCommits>({
    queryKey: ["run-commits", runId],
    queryFn: () => listRunCommits(runId!),
    enabled: !!runId,
  });

  const refresh = useCallback(() => {
    if (!runId) return;
    queryClient.invalidateQueries({ queryKey: ["run-commits", runId] });
  }, [queryClient, runId]);

  const lastSeenSeqRef = useRef<number>(-1);
  const debounceRef = useRef<ReturnType<typeof setTimeout> | null>(null);

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
