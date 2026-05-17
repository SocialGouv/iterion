// useWhatsNextSession is the glue between PiloteView and the run
// engine. It owns the runId state, launches a fresh session via
// `createRun`, subscribes to the run's WS to fold events into messages,
// and exposes a `submitHumanAnswer` callback for chat inputs.
//
// State is intentionally local to the hook (not a Zustand store) so
// the Pilote view can be navigated away from and remounted without
// resurrecting a stale session. Auto-attach to an already-running
// session is wired in Étape 5 — for now, mounting the hook with
// `null` keeps it idle.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import {
  createRun,
  getRun,
  resumeRun,
  type RunStatus,
} from "@/api/runs";
import { useRunWebSocket } from "@/hooks/useRunWebSocket";
import type { FirstClassBot } from "@/lib/pilote/firstClassBots";
import type { PiloteMessage } from "@/lib/pilote/messages";
import { useRunStore } from "@/store/run";
import { useServerInfoStore } from "@/store/serverInfo";

import { messagesFromEvents } from "./messagesFromEvents";
import {
  forgetSessionRunId,
  recallSessionRunId,
  rememberSessionRunId,
} from "./sessionStorage";

// Run statuses considered "live" enough to auto-attach to. A
// `finished` / `failed` / `cancelled` run is forgotten on the next
// mount so the launcher comes back fresh.
const LIVE_STATUSES: ReadonlySet<RunStatus> = new Set<RunStatus>([
  "queued",
  "running",
  "paused_waiting_human",
  "failed_resumable",
]);

export type WhatsNextStatus =
  | "idle"
  | "launching"
  | "active"
  | "submitting"
  | "ended";

export interface UseWhatsNextSession {
  status: WhatsNextStatus;
  // The current run id, or null when no session is active.
  runId: string | null;
  // The derived chat transcript.
  messages: PiloteMessage[];
  // The id of the human message currently being submitted (drives
  // the busy state on the matching chat-turn). Null when no submit
  // is in flight.
  busyMessageId: string | null;
  // The raw RunStatus from the latest snapshot, exposed for UIs
  // that want to surface details beyond the high-level status.
  runStatus: RunStatus | null;
  // Total number of run events the store has consumed for this run.
  // PreFlightPanel uses it to differentiate "no events yet" from
  // "events arrived but didn't map to a known node".
  rawEventCount: number;
  // Last error from launch/submit, if any.
  errorMessage: string | null;
  // Imperative actions.
  launch: (vars: Record<string, string>) => Promise<void>;
  submitHumanAnswer: (
    messageId: string,
    answers: Record<string, unknown>,
  ) => Promise<void>;
}

export function useWhatsNextSession(bot: FirstClassBot): UseWhatsNextSession {
  const [runId, setRunId] = useState<string | null>(null);
  const [status, setStatus] = useState<WhatsNextStatus>("idle");
  const [busyMessageId, setBusyMessageId] = useState<string | null>(null);
  const [errorMessage, setErrorMessage] = useState<string | null>(null);

  // Subscribe to the WS for the active run. The hook is a no-op when
  // runId is null. The store is shared with the rest of the SPA — a
  // user who flips to /runs/:id mid-session will see the same data,
  // and our reads of useRunStore here pick up the same updates.
  useRunWebSocket(runId);

  // Auto-attach: on mount, if we remembered a runId for this bot+project,
  // try to fetch its snapshot and (if it's still live) attach. Otherwise
  // forget the stale id.
  const projectId = useServerInfoStore((s) => s.info?.current_project_id ?? null);
  const attachAttemptedRef = useRef(false);
  useEffect(() => {
    if (attachAttemptedRef.current) return;
    if (!bot.id) return;
    attachAttemptedRef.current = true;
    const remembered = recallSessionRunId(bot.id, projectId);
    if (!remembered) return;
    let cancelled = false;
    setStatus("launching");
    getRun(remembered)
      .then(async (snap) => {
        if (cancelled) return;
        if (!LIVE_STATUSES.has(snap.run.status)) {
          forgetSessionRunId(bot.id, projectId);
          setStatus("idle");
          return;
        }
        useRunStore.getState().reset();
        useRunStore.getState().applySnapshot(snap);
        // setRunId on the store FIRST so loadEventHistoryIfMissing's
        // post-await guard (`state.runId !== runId` → return) passes.
        // Without this the fetched events would be silently dropped.
        useRunStore.getState().setRunId(remembered);
        // Pull the persisted event history so the transcript reflects
        // everything that happened before this mount. RunView lazy-loads
        // this only when the user opens the Events tab; for Pilote the
        // transcript IS the rendering, so we always need the full log.
        // Best-effort: failures fall through to the live-tail-only path.
        try {
          await useRunStore
            .getState()
            .loadEventHistoryIfMissing(remembered);
        } catch {
          // ignore — the live WS will eventually fill any gap.
        }
        setRunId(remembered);
      })
      .catch(() => {
        // Run no longer exists (rotated, store wiped). Drop the
        // memory so we don't keep retrying.
        if (cancelled) return;
        forgetSessionRunId(bot.id, projectId);
        setStatus("idle");
      });
    return () => {
      cancelled = true;
    };
  }, [bot.id, projectId]);

  // Reset to "idle" state when runId becomes null (Étape 5 will let
  // the user start fresh after a session-closed message).
  useEffect(() => {
    if (runId === null) {
      setStatus("idle");
      setBusyMessageId(null);
      setErrorMessage(null);
    }
  }, [runId]);

  // Read the run store's events + snapshot via selectors. Both are
  // stable references when unchanged, so React only re-renders this
  // hook's consumers when something actually moved.
  const events = useRunStore((s) => s.events);
  const snapshot = useRunStore((s) => s.snapshot);
  const pendingHuman = useRunStore((s) => s.pendingHumanInput);
  const setRunStatus = useRunStore((s) => s.setRunStatus);
  const requestWsReconnect = useRunStore((s) => s.requestWsReconnect);
  const applySnapshot = useRunStore((s) => s.applySnapshot);
  const reset = useRunStore((s) => s.reset);
  const setStoreRunId = useRunStore((s) => s.setRunId);
  const loadEventHistoryIfMissing = useRunStore(
    (s) => s.loadEventHistoryIfMissing,
  );

  // Mirror our local runId onto the run store so its actions that gate
  // on store.runId (loadEventHistoryIfMissing, setRunStatus, etc.)
  // actually fire. Without this the store stays at runId=null and
  // applyEventsBatch silently no-ops after the await fence.
  useEffect(() => {
    setStoreRunId(runId);
    return () => {
      setStoreRunId(null);
    };
  }, [runId, setStoreRunId]);

  // Promote the run status to our high-level status. The transitions:
  //   queued | running                  → active
  //   paused_waiting_human              → active (UI shows pending turn)
  //   finished | failed | cancelled | failed_resumable → ended
  // We don't transition out of "submitting" automatically — the submit
  // action does that explicitly when the WS catches up.
  const runStatus = snapshot?.run.status ?? null;
  useEffect(() => {
    if (!runId) return;
    if (!runStatus) return;
    if (status === "submitting") return;
    if (
      runStatus === "finished" ||
      runStatus === "failed" ||
      runStatus === "cancelled" ||
      runStatus === "failed_resumable"
    ) {
      setStatus("ended");
      // Once a session ends, forget the run id so the next visit to
      // /pilote presents a fresh launcher. The transcript stays on
      // screen (it lives in component state) until the user navigates.
      forgetSessionRunId(bot.id, projectId);
    } else {
      setStatus("active");
    }
  }, [bot.id, projectId, runId, runStatus, status]);

  // Derive the transcript. Memoised on the array identity of events
  // (Zustand swaps the reference on each fold) + snapshot identity.
  const messages = useMemo(
    () => messagesFromEvents({ bot, events, snapshot }),
    [bot, events, snapshot],
  );

  // Track the latest pending human message id so submitHumanAnswer
  // can route to the right turn without the caller having to look it
  // up. We keep both the id and the node_id (used by resumeRun) in a
  // ref so the submit callback stays stable.
  const pendingRef = useRef<{ messageId: string; nodeId: string } | null>(null);
  useEffect(() => {
    if (!pendingHuman?.node_id) {
      pendingRef.current = null;
      return;
    }
    // Match the same id rule as messagesFromEvents
    // (`${nodeId}:${iter}:question`). We don't have the iter from
    // pendingHuman, but the latest pending in `messages` is the only
    // one in "pending" state — find and stash it.
    const latestPending = [...messages]
      .reverse()
      .find((m) => m.kind === "human-question" && m.status === "pending");
    if (latestPending && latestPending.kind === "human-question") {
      pendingRef.current = {
        messageId: latestPending.id,
        nodeId: latestPending.nodeId,
      };
    }
  }, [pendingHuman, messages]);

  const launch = useCallback(
    async (vars: Record<string, string>) => {
      setErrorMessage(null);
      setStatus("launching");
      // Make sure we start from a clean store: the editor session may
      // have a previous run loaded.
      reset();
      try {
        const res = await createRun({
          file_path: bot.workflowPath,
          vars,
        });
        rememberSessionRunId(bot.id, projectId, res.run_id);
        // Pin the store's runId early so loadEventHistoryIfMissing's
        // `state.runId !== runId` guard doesn't drop the fetched batch
        // after its await — same trick the auto-attach branch uses.
        useRunStore.getState().setRunId(res.run_id);
        setRunId(res.run_id);
        // Seed the store with the freshly-created run's snapshot AND
        // any events the runtime persisted between createRun and now.
        // Without the second call, the WS subscribes at snap.last_seq+1
        // and silently misses everything up to that point — typically
        // run_started + the first node_started, which leaves the
        // transcript blank until propose_roadmap fires.
        try {
          const snap = await getRun(res.run_id);
          applySnapshot(snap);
          await loadEventHistoryIfMissing(res.run_id);
        } catch {
          // ignore; the WS will deliver the snapshot.
        }
      } catch (e) {
        setErrorMessage((e as Error).message);
        setStatus("idle");
      }
    },
    [
      bot.workflowPath,
      bot.id,
      projectId,
      reset,
      applySnapshot,
      loadEventHistoryIfMissing,
    ],
  );

  const submitHumanAnswer = useCallback(
    async (messageId: string, answers: Record<string, unknown>) => {
      if (!runId) return;
      setErrorMessage(null);
      setBusyMessageId(messageId);
      setStatus("submitting");
      try {
        await resumeRun(runId, { answers });
        setRunStatus("running");
        // Re-dial the WS so the resumed engine's events reach us.
        // Without this, the broker may have dropped subscribers when
        // the run went paused_waiting_human and the live tail stays
        // silent. Same trick the HumanInteractionPanel uses.
        requestWsReconnect();
        // Belt-and-braces: refresh the snapshot ~600ms later so a
        // short-lived run that finishes before the WS redial still
        // surfaces a final state.
        window.setTimeout(() => {
          getRun(runId)
            .then(applySnapshot)
            .catch(() => {});
        }, 600);
        setStatus("active");
      } catch (e) {
        setErrorMessage((e as Error).message);
        setStatus("active");
      } finally {
        setBusyMessageId(null);
      }
    },
    [runId, setRunStatus, requestWsReconnect, applySnapshot],
  );

  return {
    status,
    runId,
    messages,
    busyMessageId,
    runStatus,
    rawEventCount: events.length,
    errorMessage,
    launch,
    submitHumanAnswer,
  };
}
