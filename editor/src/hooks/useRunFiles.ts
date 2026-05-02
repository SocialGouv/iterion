import { useCallback, useEffect, useRef, useState } from "react";

import { listRunFiles, type RunFiles } from "@/api/runs";
import { useRunStore } from "@/store/run";

// useRunFiles fetches the modified-files listing for runId and auto-
// refetches whenever an event suggests the working tree just changed
// (node_finished, run_finished, run_failed, run_cancelled, run_paused).
// A single fetch is debounced over a 300ms window to coalesce bursts —
// fan-out branches finishing back-to-back would otherwise trigger a
// flurry of identical requests.
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

export function useRunFiles(runId: string | null): {
  data: RunFiles | null;
  loading: boolean;
  error: string | null;
  refresh: () => void;
} {
  const [data, setData] = useState<RunFiles | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // High-water mark on event seq so we only react to new events, not
  // re-react on every store update.
  const lastSeenSeqRef = useRef<number>(-1);
  const debounceRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  // Race guard: a slow request whose runId is no longer active must
  // not overwrite the state of the new run.
  const genRef = useRef(0);

  const events = useRunStore((s) => s.events);

  const fetchNow = useCallback(() => {
    if (!runId) return;
    const myGen = ++genRef.current;
    setLoading(true);
    listRunFiles(runId)
      .then((res) => {
        if (myGen !== genRef.current) return;
        setData(res);
        setError(null);
      })
      .catch((err: unknown) => {
        if (myGen !== genRef.current) return;
        setError(err instanceof Error ? err.message : "Failed to load files");
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
    // Always advance the marker so we don't re-process old events even
    // when the latest batch contains nothing relevant.
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
