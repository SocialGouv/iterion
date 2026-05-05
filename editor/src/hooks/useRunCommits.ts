import { useCallback, useEffect, useRef, useState } from "react";

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
  const [data, setData] = useState<RunCommits | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const lastSeenSeqRef = useRef<number>(-1);
  const debounceRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const genRef = useRef(0);

  const events = useRunStore((s) => s.events);

  const fetchNow = useCallback(() => {
    if (!runId) return;
    const myGen = ++genRef.current;
    setLoading(true);
    listRunCommits(runId)
      .then((res) => {
        if (myGen !== genRef.current) return;
        setData(res);
        setError(null);
      })
      .catch((err: unknown) => {
        if (myGen !== genRef.current) return;
        setError(err instanceof Error ? err.message : "Failed to load commits");
      })
      .finally(() => {
        if (myGen !== genRef.current) return;
        setLoading(false);
      });
  }, [runId]);

  useEffect(() => {
    if (!runId) {
      setData(null);
      setError(null);
      lastSeenSeqRef.current = -1;
      return;
    }
    lastSeenSeqRef.current = -1;
    fetchNow();
  }, [runId, fetchNow]);

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
      fetchNow();
    }, DEBOUNCE_MS);
    return () => {
      if (debounceRef.current) {
        clearTimeout(debounceRef.current);
        debounceRef.current = null;
      }
    };
  }, [events, runId, fetchNow]);

  return { data, loading, error, refresh: fetchNow };
}
