// What the assistant is STANDING BY on, and the one call that stops it.
//
// The dock cannot read this off the run snapshot: the snapshot reducer is
// deterministic over (run.json, events) — the frontend replays it locally to
// power the time-travel scrubber — so a field fed by the separate runwatch
// store would diverge between server and client. The server therefore emits
// assistant_veille_armed / _stopped as ordinary run events, and this hook
// uses them as a doorbell: an event arrives, we re-read the authoritative
// state. Fast path plus reconciliation, the same discipline the board uses.

import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";

import {
  listRunVeille,
  removeWatch,
  stopAssistantRunWatch,
  type RunVeille,
} from "@/api/runs";
import { useRunStore } from "@/store/run";

const EMPTY: RunVeille = { run_watches: [], watched_issue_ids: [] };

export interface UseRunVeille {
  // active is the single question the banner asks. Either channel counts:
  // both make the assistant speak without being addressed.
  active: boolean;
  issueIds: string[];
  runTargets: string[];
  busy: boolean;
  error: string | null;
  // stop cuts BOTH channels. Cutting only the run watches would leave the
  // card subscription to re-arm one at the next dispatch.
  stop: () => Promise<void>;
}

interface VeilleScope {
  runId: string;
  alive: boolean;
  request: number;
  stopping: boolean;
}

interface VeilleState {
  runId: string | null;
  veille: RunVeille;
  busy: boolean;
  error: string | null;
}

const EMPTY_STATE: VeilleState = { runId: null, veille: EMPTY, busy: false, error: null };

export function useRunVeille(runId: string | null): UseRunVeille {
  const [state, setState] = useState<VeilleState>(EMPTY_STATE);
  const scopeRef = useRef<VeilleScope | null>(null);
  // A switch hides the previous run's data in this render. Commit the lifetime
  // separately so discarded concurrent renders cannot invalidate the live run.
  const current = state.runId === runId ? state : EMPTY_STATE;
  useLayoutEffect(() => {
    const scope = runId ? { runId, alive: true, request: 0, stopping: false } : null;
    scopeRef.current = scope;
    // Reset committed state on ownership changes, including A -> B -> A before
    // B has loaded, so A cannot inherit a completed stop's stale busy flag.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    setState(EMPTY_STATE);
    return () => { if (scope) scope.alive = false; };
  }, [runId]);

  const doorbell = useRunStore((s) =>
    s.events.reduce(
      (n, e) => e.type === "assistant_veille_armed" || e.type === "assistant_veille_stopped" ? n + 1 : n,
      0,
    ),
  );

  const refresh = useCallback(async (scope: VeilleScope) => {
    const request = ++scope.request;
    let veille = EMPTY;
    try {
      const next = await listRunVeille(scope.runId);
      veille = {
        run_watches: next.run_watches ?? [],
        watched_issue_ids: next.watched_issue_ids ?? [],
      };
    } catch {
      // A later event retries an unreadable standby. A stale failure must not
      // erase the new run's successful response.
    }
    if (!scope.alive || request !== scope.request) return;
    setState((prev) => ({
      ...(prev.runId === scope.runId ? prev : EMPTY_STATE),
      runId: scope.runId,
      veille,
    }));
  }, []);

  useEffect(() => {
    const scope = scopeRef.current;
    if (scope) void refresh(scope);
  }, [runId, doorbell, refresh]);

  const issueIds = current.veille.watched_issue_ids;
  const runTargets = useMemo(
    () => current.veille.run_watches.map((w) => w.target_run_id),
    [current.veille.run_watches],
  );

  const stop = useCallback(async () => {
    const scope = scopeRef.current;
    if (!scope?.alive || scope.runId !== runId || scope.stopping || current.runId !== runId) return;
    // Capture both halves of the ownership pair before the first await.
    const veille = current.veille;
    scope.stopping = true;
    ++scope.request;
    setState((prev) => ({ ...prev, busy: true, error: null }));
    try {
      await Promise.all(veille.run_watches.map((w) => stopAssistantRunWatch(w.id)));
      await Promise.all(veille.watched_issue_ids.map((id) => removeWatch(scope.runId, id)));
    } catch (e) {
      if (scope.alive) setState((prev) => ({ ...prev, error: e instanceof Error ? e.message : String(e) }));
    } finally {
      if (scope.alive) {
        // Partial success also needs a fresh authoritative result.
        await refresh(scope);
        if (scope.alive) setState((prev) => ({ ...prev, busy: false }));
      }
      scope.stopping = false;
    }
  }, [runId, current, refresh]);

  return {
    active: issueIds.length > 0 || runTargets.length > 0,
    issueIds,
    runTargets,
    busy: current.busy,
    error: current.error,
    stop,
  };
}
