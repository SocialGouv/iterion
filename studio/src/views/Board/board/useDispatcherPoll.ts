import { useEffect, useRef, useState } from "react";

import {
  getState,
  type DispatchSkipView,
  type RetryView,
  type RunningView,
} from "@/api/dispatcher";
import { listIssues, type NativeIssue } from "@/api/native";

export interface UseDispatcherPollResult {
  runningByIssue: Map<string, RunningView>;
  retryingByIssue: Map<string, RetryView>;
  skipByIssue: Map<string, DispatchSkipView>;
  trackerError: { tracker: string; message: string } | null;
  dispatcherPaused: boolean;
}

// Poll the dispatcher snapshot every 2s so each card can show a
// running/retrying badge + cancel button. We ignore failures: when
// the dispatcher is idle the snapshot is still returned (empty
// running/retries), and a 5xx is rare enough that flashing the maps
// empty would be more disruptive than keeping stale data.
//
// When the active (running + retrying) set changes — a dispatch
// started or a run finished — the affected issue's server-side
// `state` has moved (ready→in_progress, →review/done), but the
// poll above only refreshed the overlay. Re-fetch issues so the
// card actually changes columns. Gated on a set *change* via
// `prevActiveSigRef` so we don't re-fetch every 2s or fight an
// in-flight optimistic drag.
export function useDispatcherPoll(
  setIssues: React.Dispatch<React.SetStateAction<NativeIssue[]>>,
  // Skip polling entirely when the dispatcher feature is off (cloud mode):
  // /api/v1/dispatcher/* isn't wired, so a 2s poll would just 404 forever.
  enabled = true,
): UseDispatcherPollResult {
  const [runningByIssue, setRunningByIssue] = useState<Map<string, RunningView>>(
    new Map(),
  );
  const [retryingByIssue, setRetryingByIssue] = useState<Map<string, RetryView>>(
    new Map(),
  );
  const [skipByIssue, setSkipByIssue] = useState<Map<string, DispatchSkipView>>(
    new Map(),
  );
  const [trackerError, setTrackerError] = useState<
    { tracker: string; message: string } | null
  >(null);
  const [dispatcherPaused, setDispatcherPaused] = useState(false);
  // Signature of the last-seen active (running + retrying) issue set.
  const prevActiveSigRef = useRef<string>("");

  useEffect(() => {
    if (!enabled) return;
    let alive = true;
    let inflight = false;
    let gen = 0;
    const tick = async () => {
      if (!alive || inflight) return;
      inflight = true;
      const myGen = ++gen;
      try {
        const snap = await getState();
        // Drop responses that arrive after a newer request has
        // started — without the gen guard, a slow getState resolving
        // after a fresh tick would clobber the newer state.
        if (!alive || myGen !== gen) return;
        const rmap = new Map<string, RunningView>();
        for (const r of snap.running ?? []) rmap.set(r.issue_id, r);
        const xmap = new Map<string, RetryView>();
        for (const r of snap.retries ?? []) xmap.set(r.issue_id, r);
        const skmap = new Map<string, DispatchSkipView>();
        for (const s of snap.dispatch_skips ?? []) skmap.set(s.issue_id, s);
        setRunningByIssue(rmap);
        setRetryingByIssue(xmap);
        setSkipByIssue(skmap);
        // Tag each id with its map. A running→retry move keeps the same
        // issue id but the dispatcher reverts its server-side state
        // (in_progress→ready); an untagged union still contains that id, so
        // the signature wouldn't change and the card would linger in the
        // in_progress column while the server has it back in ready. Tagging
        // makes "r:id"→"x:id" a signature change, forcing the re-fetch that
        // snaps the card to its real column.
        const activeSig = [
          ...[...rmap.keys()].map((id) => "r:" + id),
          ...[...xmap.keys()].map((id) => "x:" + id),
        ]
          .sort()
          .join(",");
        if (activeSig !== prevActiveSigRef.current) {
          void listIssues()
            .then((fresh) => {
              if (!alive) return;
              setIssues(fresh ?? []);
              prevActiveSigRef.current = activeSig;
            })
            .catch(() => {
              /* leave prevActiveSigRef stale → retry next tick */
            });
        }
        // Guard the tracker error update on value equality so a stable
        // poll doesn't churn an identical object reference each tick
        // and re-render the whole board.
        setTrackerError((prev) => {
          const message = snap.last_tracker_error ?? "";
          if (!message) return prev === null ? prev : null;
          if (
            prev &&
            prev.tracker === snap.tracker &&
            prev.message === message
          ) {
            return prev;
          }
          return { tracker: snap.tracker, message };
        });
        setDispatcherPaused(!!snap.paused);
      } catch {
        // swallow: dispatcher may be unreachable / not wired
      } finally {
        inflight = false;
      }
    };
    void tick();
    const id = setInterval(() => void tick(), 2000);
    return () => {
      alive = false;
      clearInterval(id);
    };
  }, [setIssues, enabled]);

  return {
    runningByIssue,
    retryingByIssue,
    skipByIssue,
    trackerError,
    dispatcherPaused,
  };
}
