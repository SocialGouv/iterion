// useWhatsNextSession is the glue between WhatsNextView and the run
// engine. It owns the runId state, launches a fresh session via
// `createRun`, subscribes to the run's WS to fold events into messages,
// and exposes a `submitHumanAnswer` callback for chat inputs.
//
// State is local to the hook rather than a Zustand store, and the hook
// is mounted ONCE above the route tree (AssistantProvider) rather than
// by a view. Navigation therefore neither restarts the session nor
// drops the transcript — see ADR-087. What used to bound a stale
// session (the view unmounting) is now explicit: the scope-change
// effect below drops a session that belongs to another project/repo.
//
// The hook is a composition shell over the concern modules in this
// directory:
//   - sessionScope        — repo-first scope key + launch repo
//   - useSessionDiscovery — startup auto-attach (live run > remembered)
//   - useSessionMessages  — event fold → transcript + pause-gap refetch
//   - useSessionLifecycle — launch / newSession
//   - useSessionSteering  — submitHumanAnswer / resume choreography
//   - sessionStatus       — WhatsNextStatus + terminal classification

import { useEffect, useRef, useState } from "react";

import { type RunStatus } from "@/api/runs";
import { useRunWebSocket } from "@/hooks/useRunWebSocket";
import type { FirstClassBot } from "@/lib/whats-next/firstClassBots";
import type { WhatsNextMessage } from "@/lib/whats-next/messages";
import { useRunStore, useRunStoreInstance } from "@/store/run";

import { useSessionScope } from "./sessionScope";
import { isEndedRunStatus, type WhatsNextStatus } from "./sessionStatus";
import { useSessionDiscovery } from "./useSessionDiscovery";
import { useSessionLifecycle } from "./useSessionLifecycle";
import { useSessionMessages } from "./useSessionMessages";
import { useSessionSteering } from "./useSessionSteering";

export type { WhatsNextStatus } from "./sessionStatus";

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
  // Last error from launch/submit, if any.
  errorMessage: string | null;
  // The vars used to launch (or that would launch) the current
  // session. Exposed so the view can re-seed a fresh run with the
  // same scope after the previous one closed. Null before any launch
  // in this mount (e.g. after auto-attach), in which case the bot's
  // var defaults apply.
  lastVars: Record<string, string> | null;
  // Startup discovery failure (couldn't list runs to find a live
  // session). Surfaced so the launcher can warn: launching blind may
  // start a second parallel session. Null when discovery succeeded.
  discoveryError: string | null;
  // Re-run the startup discovery after a failure.
  retryDiscovery: () => void;
  // The attached session's repo identity (run.project_path), for the
  // header's repo pill + scope-mismatch banner. Null when unknown or
  // repo-less.
  sessionRepo: string | null;
  // The repo the NEXT launch will operate on (cloud: the sidebar's
  // active repo). Null = board-only launch (no repo connected / overview).
  launchRepo: string | null;
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

  // Hook-lifetime abort handle for the launch path's getRunWithRetry:
  // without it, an abandoned launch (user navigates away mid-launch)
  // keeps its 404-backoff loop ticking against the network for up to
  // ~5s after unmount. Created per mount inside the effect (not in the
  // ref initializer) so StrictMode's simulated unmount doesn't leave a
  // permanently-aborted controller behind for the real mount.
  const lifetimeAbortRef = useRef<AbortController | null>(null);
  useEffect(() => {
    const controller = new AbortController();
    lifetimeAbortRef.current = controller;
    return () => {
      controller.abort();
      if (lifetimeAbortRef.current === controller) {
        lifetimeAbortRef.current = null;
      }
    };
  }, []);

  // Subscribe to the WS for the active run. The hook is a no-op when
  // runId is null. Reads and writes go to whichever store the hook is
  // mounted under — the assistant's isolated one below AssistantProvider,
  // the module default otherwise.
  useRunWebSocket(runId);
  const store = useRunStoreInstance();

  // Repo-first scope: (team, active repo) in cloud, project dir
  // locally. The key participates in session storage + discovery.
  const { scopeKey, repoScopeEnabled, overview, activeRepo, projectId, launchRepo } =
    useSessionScope();

  // A scope change (project switch, or the cloud sidebar's active repo)
  // means the attached session belongs to somewhere the operator no
  // longer is. Drop it so discovery re-runs clean for the new scope.
  //
  // This used to be covered incidentally: the view unmounted on the
  // navigate-to-"/" that follows a project switch, taking the session's
  // state with it. The session is mounted above the route tree now, so
  // nothing unmounts and a stale conversation would simply stay on
  // screen — discovery only overwrites runId when it FINDS a run.
  //
  // The old scope's remembered runId is deliberately left in place, so
  // switching back re-attaches instead of starting over.
  const prevScopeRef = useRef<string | null | undefined>(undefined);
  useEffect(() => {
    const prev = prevScopeRef.current;
    prevScopeRef.current = scopeKey;
    // First resolution (undefined → the real key) is not a switch.
    if (prev === undefined || prev === scopeKey) return;
    setRunId(null);
    setStatus("idle");
    store.getState().reset();
  }, [scopeKey, store]);

  // Startup auto-attach: live run first, remembered run second, idle
  // launcher last. Owns discoveryError + retry.
  const { discoveryError, retryDiscovery } = useSessionDiscovery({
    bot,
    scopeKey,
    repoScopeEnabled,
    overview,
    activeRepo,
    onAttached: setRunId,
    setStatus,
  });

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
  const setStoreRunId = useRunStore((s) => s.setRunId);

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
    // Keep the runId in localStorage in every terminal state so the
    // next visit re-hydrates the transcript — continuity is the
    // central whats-next promise (full exchange visible across app
    // restarts). A finished run (the operator hit `close`) still keeps
    // a live composer via the view's always-on footer, which re-seeds
    // a fresh session on the next message — so "ended" is no longer a
    // dead end. The user can also start over via newSession().
    if (isEndedRunStatus(runStatus)) {
      setStatus("ended");
    } else {
      setStatus("active");
    }
  }, [bot.id, projectId, runId, runStatus, status]);

  // Transcript fold + pending-question tracking + pause-gap refetch.
  const messages = useSessionMessages({
    bot,
    runId,
    runStatus,
    events,
    snapshot,
    pendingHuman,
  });

  const { launch, newSession, lastVarsRef } = useSessionLifecycle({
    bot,
    scopeKey,
    repoScopeEnabled,
    activeRepo,
    lifetimeAbortRef,
    setRunId,
    setStatus,
    setBusyMessageId,
    setErrorMessage,
  });

  const { submitHumanAnswer, resume } = useSessionSteering({
    runId,
    setStatus,
    setBusyMessageId,
    setErrorMessage,
  });

  return {
    status,
    runId,
    messages,
    busyMessageId,
    runStatus,
    errorMessage,
    lastVars: lastVarsRef.current,
    discoveryError,
    retryDiscovery,
    sessionRepo: snapshot?.run.project_path ?? null,
    launchRepo,
    launch,
    submitHumanAnswer,
    newSession,
    resume,
  };
}
