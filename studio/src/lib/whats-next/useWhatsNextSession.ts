// useWhatsNextSession is the glue between WhatsNextView and the run
// engine. It owns the runId state, launches a fresh session via
// `createRun`, subscribes to the run's WS to fold events into messages,
// and exposes a `submitHumanAnswer` callback for chat inputs.
//
// State is intentionally local to the hook (not a Zustand store) so
// the WhatsNext view can be navigated away from and remounted without
// resurrecting a stale session. Auto-attach to an already-running
// session is wired in Étape 5 — for now, mounting the hook with
// `null` keeps it idle.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import {
  createRun,
  getRun,
  listRuns,
  resumeRun,
  type RunStatus,
  type RunSummary,
} from "@/api/runs";
import { useRunWebSocket } from "@/hooks/useRunWebSocket";
import type { FirstClassBot } from "@/lib/whats-next/firstClassBots";
import type { WhatsNextMessage } from "@/lib/whats-next/messages";
import { runStore, useRunStore } from "@/store/run";
import { useServerInfoStore } from "@/store/serverInfo";

import {
  messagesFromEventsCached,
  type MessagesFoldCache,
} from "./messagesFromEvents";
import {
  forgetSessionRunId,
  recallSessionRunId,
  rememberSessionRunId,
} from "./sessionStorage";

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
  messages: WhatsNextMessage[];
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
  // Clear the currently-attached session (forgetting its runId from
  // localStorage) and return to the idle state so the SessionLauncher
  // re-appears. The transcript of the prior session is dropped — use
  // when the user explicitly wants to start a fresh exchange.
  newSession: () => void;
  // Re-enter a failed_resumable / cancelled run from its checkpoint
  // without supplying new human answers. Used by the SessionHeader's
  // Resume button so the operator doesn't have to flip to /runs/<id>
  // to recover from a transient backend error (the rest of the
  // submit machinery already lives on submitHumanAnswer).
  resume: () => Promise<void>;
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
  //
  // Fallback path: when localStorage is empty (fresh tab on the dev
  // origin, freshly-cleared storage, different origin from the run's
  // launch context), query the backend for the most recent non-terminal
  // run on this bot's workflow and auto-attach to it. This means an
  // operator who closed their /whats-next tab while a run was in
  // flight can navigate back and resume without having to dig the
  // run id out of /runs.
  const projectId = useServerInfoStore((s) => s.info?.current_project_id ?? null);
  const attachAttemptedRef = useRef(false);
  useEffect(() => {
    if (attachAttemptedRef.current) return;
    if (!bot.id) return;
    attachAttemptedRef.current = true;
    const controller = new AbortController();
    let cancelled = false;

    const attachTo = async (runIdToAttach: string) => {
      const snap = await getRun(runIdToAttach, { signal: controller.signal });
      if (cancelled) return;
      // Continuity is the central whats-next promise: when the user
      // returns to /whats-next after a previous session ended, they
      // expect to see the full transcript of that exchange, not a
      // blank launcher offering them to start over.
      runStore.getState().reset();
      runStore.getState().applySnapshot(snap);
      // setRunId on the store FIRST so loadEventHistoryIfMissing's
      // post-await guard (`state.runId !== runId` → return) passes.
      runStore.getState().setRunId(runIdToAttach);
      try {
        await runStore.getState().loadEventHistoryIfMissing(runIdToAttach);
      } catch {
        // ignore — the live WS will eventually fill any gap.
      }
      // Remember now so subsequent mounts (within the same origin)
      // skip the discovery query and re-attach via localStorage.
      rememberSessionRunId(bot.id, projectId, runIdToAttach);
      setRunId(runIdToAttach);
    };

    const remembered = recallSessionRunId(bot.id, projectId);
    setStatus("launching");
    const startup = remembered
      ? attachTo(remembered).catch((err) => {
          if (
            controller.signal.aborted ||
            (err as Error)?.name === "AbortError"
          ) {
            return;
          }
          // Run no longer exists (rotated, store wiped). Drop the
          // memory and fall through to server-side discovery so the
          // operator doesn't get stuck on a stale localStorage entry.
          forgetSessionRunId(bot.id, projectId);
          return discoverAndAttach();
        })
      : discoverAndAttach();

    async function discoverAndAttach() {
      try {
        // Look for the most recent non-terminal whats-next run.
        // workflow_name comes from the DSL's `workflow whats_next:`
        // declaration — by convention the FirstClassBot's id matches
        // the workflow name with hyphens vs underscores (e.g.
        // "whats-next" ↔ "whats_next"). We try both.
        const candidates = [bot.id.replace(/-/g, "_"), bot.id];
        const seen = new Set<string>();
        const matches: RunSummary[] = [];
        for (const workflow of candidates) {
          if (seen.has(workflow)) continue;
          seen.add(workflow);
          const runs = await listRuns({ workflow, limit: 10 });
          matches.push(...runs);
        }
        if (typeof console !== "undefined") {
          console.debug("[whats-next] discoverAndAttach: scanned", {
            botId: bot.id,
            candidates,
            matchCount: matches.length,
            statuses: matches.map((r) => `${r.id.slice(0, 8)}:${r.status}`),
          });
        }
        // Pick the most recent non-terminal one. RunSummary lists are
        // already sorted newest-first by the server.
        const active = matches.find(
          (r) =>
            r.status === "queued" ||
            r.status === "running" ||
            r.status === "paused_waiting_human",
        );
        if (!active) {
          if (typeof console !== "undefined") {
            console.debug(
              "[whats-next] discoverAndAttach: no active run, idling",
            );
          }
          setStatus("idle");
          return;
        }
        if (typeof console !== "undefined") {
          console.debug("[whats-next] discoverAndAttach: attaching to", {
            id: active.id,
            status: active.status,
          });
        }
        await attachTo(active.id);
      } catch (err) {
        if (
          controller.signal.aborted ||
          (err as Error)?.name === "AbortError"
        ) {
          return;
        }
        if (typeof console !== "undefined") {
          console.warn("[whats-next] discoverAndAttach failed", err);
        }
        // Discovery failed — fall back to launcher. Operator can
        // still start a fresh session manually.
        setStatus("idle");
      }
    }

    void startup;
    return () => {
      cancelled = true;
      controller.abort();
      // Reset the gate so the strict-mode double-mount (and any
      // legitimate re-mount triggered by deps changing) re-runs the
      // discovery cleanly. Without this, mount #2's effect short-
      // circuits, mount #1's aborted discovery never sets runId, and
      // status stays "idle" → the launcher mistakenly shows even
      // when a paused run is sitting on disk waiting to be resumed.
      attachAttemptedRef.current = false;
    };
  }, [bot.id, projectId]);

  // Reset to "idle" state when runId becomes null (Étape 5 lets the
  // user start fresh after a session-closed message via newSession()).
  //
  // Critically: don't override "launching" — the auto-attach effect
  // sets that synchronously while its async discovery / hydration is
  // in flight, and a too-eager "idle" flip here makes the launcher
  // briefly flash (or stick, if the discovery aborts). Only reset
  // when we're already in a terminal/active state that the null
  // runId would be inconsistent with.
  useEffect(() => {
    if (runId !== null) return;
    setStatus((s) => (s === "launching" ? s : "idle"));
    setBusyMessageId(null);
    setErrorMessage(null);
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
  //
  // Crucially: do NOT reset the store on unmount. A common navigation
  // pattern (WhatsNext → RunView console for the same run id) mounts the
  // destination consumer at almost the same instant WhatsNext unmounts;
  // the null-reset here would briefly clear store.runId and any
  // inflight loadEventHistoryIfMissing await would drop events on the
  // floor when its post-await guard re-reads state.runId. Leave the
  // store's runId untouched — the next consumer's mount writes the
  // correct id; explicit reset() is the user's WhatsNext → Home path.
  useEffect(() => {
    setStoreRunId(runId);
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
      // Keep the runId in localStorage so the next visit re-hydrates
      // the transcript — continuity is the central whats-next promise
      // (full exchange visible across app restarts). The user starts
      // a fresh session via the explicit "New session" button exposed
      // through newSession() (which clears localStorage + state).
      setStatus("ended");
    } else {
      setStatus("active");
    }
  }, [bot.id, projectId, runId, runStatus, status]);

  // Derive the transcript with an incremental fold. The cached folder
  // resumes from the last processed seq instead of replaying the whole
  // event stream every push (O(K) per tick instead of O(N)). The cache
  // is invalidated implicitly when bot changes (new session) or when
  // the first event seq differs (full replay after reconnect).
  // Snapshot updates don't invalidate: the fold reads snapshot only at
  // node_finished time and bakes the summary into the message, so a
  // resumed fold under a stale snapshot produces the same output a
  // fresh refold would have.
  const transcriptCacheRef = useRef<MessagesFoldCache | null>(null);
  const messages = useMemo(() => {
    const { messages: out, cache } = messagesFromEventsCached(
      { bot, events, snapshot },
      transcriptCacheRef.current,
    );
    transcriptCacheRef.current = cache;
    // Return a fresh array reference so memo consumers see a new value
    // when new events land. (Mutating `out` in place wouldn't propagate
    // through React.)
    return out.slice();
  }, [bot, events, snapshot]);

  // Track the latest pending human message id so submitHumanAnswer
  // can route to the right turn without the caller having to look it
  // up. We keep both the id and the node_id (used by resumeRun) in a
  // ref so the submit callback stays stable.
  const pendingRef = useRef<{ messageId: string; nodeId: string } | null>(null);

  // Belt-and-braces snapshot refresh scheduled by submitHumanAnswer.
  // Held in a ref so we can cancel a pending timer when the hook
  // unmounts or when a new submit supersedes the previous one — without
  // this, a fast WhatsNext-to-WhatsNext navigation would let an old timer
  // apply a stale snapshot to a different bot's session.
  const refreshTimerRef = useRef<number | null>(null);
  useEffect(() => {
    return () => {
      if (refreshTimerRef.current !== null) {
        window.clearTimeout(refreshTimerRef.current);
        refreshTimerRef.current = null;
      }
    };
  }, []);
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
      // Make sure we start from a clean store: the studio session may
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
        runStore.getState().setRunId(res.run_id);
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

  // Explicit "start a fresh session" action. WhatsNextView wires this
  // to a header button visible when status === "ended". Without it the
  // user has no way to clear an ended session and reach the launcher
  // again (continuity by default keeps the previous run visible across
  // app restarts).
  const newSession = useCallback(() => {
    forgetSessionRunId(bot.id, projectId);
    runStore.getState().reset();
    setRunId(null);
    setBusyMessageId(null);
    setErrorMessage(null);
    // setStatus("idle") happens automatically via the runId-null effect
    // (line that watches runId for null → resets to idle).
  }, [bot.id, projectId]);

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
        // silent. Same trick the generic HumanPromptForm uses.
        requestWsReconnect();
        // Belt-and-braces: refresh the snapshot ~600ms later so a
        // short-lived run that finishes before the WS redial still
        // surfaces a final state. We capture the target runId so a
        // late-firing timer can't apply a snapshot for a stale session
        // (e.g. after WhatsNext-to-WhatsNext navigation within 600ms), and
        // we cancel any previous pending timer so only the most recent
        // submit's refresh wins.
        if (refreshTimerRef.current !== null) {
          window.clearTimeout(refreshTimerRef.current);
        }
        const targetRunId = runId;
        refreshTimerRef.current = window.setTimeout(() => {
          refreshTimerRef.current = null;
          if (runStore.getState().runId !== targetRunId) return;
          getRun(targetRunId)
            .then(applySnapshot)
            .catch((e) => {
              // The WS will recover the state, but surface the failure
              // in devtools so silent 401/5xx don't go unnoticed.
              console.warn("whats-next snapshot refresh failed", e);
            });
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

  // Bare-resume entry point: re-enter the run from its checkpoint
  // without supplying new human answers. Used for failed_resumable
  // (transient backend errors, missing tools, schema mismatches the
  // operator fixed in source) and for cancelled runs the operator
  // wants to bring back. The submit path lives on submitHumanAnswer
  // because most resumes carry user input — this one is the rarer
  // "I fixed the code, please retry" flow.
  //
  // `force: true` is intentional: the bare-resume entry point is
  // typically triggered AFTER the operator edited the bot to fix the
  // root cause of the failure, which changes the workflow hash. The
  // operator's intent ("retry with my fix") is unambiguous — we'd
  // bounce them to /runs/<id> to find the "Force resume" toggle
  // otherwise, which defeats the point of an inline button.
  const resume = useCallback(async () => {
    if (!runId) return;
    setErrorMessage(null);
    setStatus("submitting");
    try {
      await resumeRun(runId, { answers: {}, force: true });
      setRunStatus("running");
      requestWsReconnect();
      if (refreshTimerRef.current !== null) {
        window.clearTimeout(refreshTimerRef.current);
      }
      const targetRunId = runId;
      refreshTimerRef.current = window.setTimeout(() => {
        refreshTimerRef.current = null;
        if (runStore.getState().runId !== targetRunId) return;
        getRun(targetRunId)
          .then(applySnapshot)
          .catch((e) => {
            console.warn("whats-next snapshot refresh failed", e);
          });
      }, 600);
      setStatus("active");
    } catch (e) {
      setErrorMessage((e as Error).message);
      setStatus("ended");
    }
  }, [runId, setRunStatus, requestWsReconnect, applySnapshot]);

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
    newSession,
    resume,
  };
}
