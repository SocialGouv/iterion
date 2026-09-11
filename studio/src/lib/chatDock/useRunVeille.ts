// What the assistant is STANDING BY on, and the one call that stops it.
//
// The dock cannot read this off the run snapshot: the snapshot reducer is
// deterministic over (run.json, events) — the frontend replays it locally to
// power the time-travel scrubber — so a field fed by the separate runwatch
// store would diverge between server and client. The server therefore emits
// assistant_veille_armed / _stopped as ordinary run events, and this hook
// uses them as a doorbell: an event arrives, we re-read the authoritative
// state. Fast path plus reconciliation, the same discipline the board uses.

import { useCallback, useEffect, useMemo, useState } from "react";

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

export function useRunVeille(runId: string | null): UseRunVeille {
  const [veille, setVeille] = useState<RunVeille>(EMPTY);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // The doorbell. Counting the veille events rather than watching the whole
  // event array keeps the fetch out of every unrelated transcript update.
  const doorbell = useRunStore((s) =>
    s.events.reduce(
      (n, e) =>
        e.type === "assistant_veille_armed" ||
        e.type === "assistant_veille_stopped"
          ? n + 1
          : n,
      0,
    ),
  );

  const refresh = useCallback(async () => {
    if (!runId) {
      setVeille(EMPTY);
      return;
    }
    try {
      const next = await listRunVeille(runId);
      setVeille({
        run_watches: next.run_watches ?? [],
        watched_issue_ids: next.watched_issue_ids ?? [],
      });
    } catch {
      // A standby the dock cannot read is not worth an error banner: the
      // assistant still works, it just draws no chip. The next event or
      // remount retries.
      setVeille(EMPTY);
    }
  }, [runId]);

  useEffect(() => {
    void refresh();
  }, [refresh, doorbell]);

  const issueIds = veille.watched_issue_ids;
  const runTargets = useMemo(
    () => veille.run_watches.map((w) => w.target_run_id),
    [veille.run_watches],
  );

  const stop = useCallback(async () => {
    if (!runId) return;
    setBusy(true);
    setError(null);
    try {
      // Run watches first: stopping a card subscription while its watches
      // stay armed would leave the assistant waking on outcomes for a card
      // it no longer follows.
      await Promise.all(
        veille.run_watches.map((w) => stopAssistantRunWatch(w.id)),
      );
      await Promise.all(issueIds.map((id) => removeWatch(runId, id)));
      await refresh();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      // Partial success is real (one of several calls may have landed), so
      // re-read rather than assume either outcome.
      await refresh();
    } finally {
      setBusy(false);
    }
  }, [runId, veille.run_watches, issueIds, refresh]);

  return {
    active: issueIds.length > 0 || runTargets.length > 0,
    issueIds,
    runTargets,
    busy,
    error,
    stop,
  };
}
