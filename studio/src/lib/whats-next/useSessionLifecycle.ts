// Session lifecycle actions for whats-next: launch a fresh run with
// the current scope, and explicitly clear an ended session so the
// launcher re-appears.

import { useCallback, useRef } from "react";

// Aliased: the orchestrating hook binds a local `errorMessage` field.
import { errorMessage as toMessage } from "@/lib/errorHints";

import { createRun, getRunWithRetry } from "@/api/runs";
import type { ForgeTeamRepo } from "@/api/forgeConnections";
import { modelPrefOverrides } from "@/api/modelPrefs";
import type { SessionModelChoice } from "@/hooks/useSessionModelPref";
import type { FirstClassBot } from "@/lib/whats-next/firstClassBots";
import { useRunStore, useRunStoreInstance } from "@/store/run";

import type { WhatsNextStatus } from "./sessionStatus";
import { forgetSessionRunId, rememberSessionRunId } from "./sessionStorage";

export interface SessionLifecycle {
  launch: (vars: Record<string, string>) => Promise<void>;
  newSession: () => void;
  // Vars of the most recent launch, exposed so a re-seed (composer
  // send after the run closed) reuses the same scope.
  lastVarsRef: { current: Record<string, string> | null };
}

export function useSessionLifecycle(opts: {
  bot: FirstClassBot;
  scopeKey: string | null;
  repoScopeEnabled: boolean;
  activeRepo: ForgeTeamRepo | null;
  // Hook-lifetime abort handle owned by the orchestrating hook (stops
  // the launch path's getRunWithRetry 404-backoff on unmount).
  lifetimeAbortRef: { current: AbortController | null };
  // modelChoice reads the operator's remembered model/backend/effort at the
  // MOMENT of launch. It is a getter, not a value, so a choice made a keystroke
  // before pressing send is the one that ships — and so this callback does not
  // have to be re-created (dropping its identity into every consumer) each
  // time the picker moves.
  modelChoice?: () => SessionModelChoice;
  setRunId: (runId: string | null) => void;
  setStatus: (status: WhatsNextStatus) => void;
  setBusyMessageId: (id: string | null) => void;
  setErrorMessage: (msg: string | null) => void;
}): SessionLifecycle {
  const {
    bot,
    scopeKey,
    repoScopeEnabled,
    activeRepo,
    lifetimeAbortRef,
    modelChoice,
    setRunId,
    setStatus,
    setBusyMessageId,
    setErrorMessage,
  } = opts;

  // Remembers the vars of the most recent launch so a re-seed (typing
  // into the composer after the run closed) reuses the same scope.
  const lastVarsRef = useRef<Record<string, string> | null>(null);

  // The store this session lives in — the assistant's isolated one
  // when mounted under AssistantProvider, the module default
  // otherwise. Imperative reads MUST go through it: reaching for the
  // module-default `runStore` façade here would split the session's
  // state (reads via the provider, writes to another store).
  const store = useRunStoreInstance();
  const applySnapshot = useRunStore((s) => s.applySnapshot);
  const reset = useRunStore((s) => s.reset);
  const loadEventHistoryIfMissing = useRunStore(
    (s) => s.loadEventHistoryIfMissing,
  );

  const launch = useCallback(
    async (vars: Record<string, string>) => {
      setErrorMessage(null);
      setStatus("launching");
      // Remember the scope so a later re-seed (composer send after the
      // run closed) reuses it.
      lastVarsRef.current = vars;
      // Make sure we start from a clean store: the studio session may
      // have a previous run loaded.
      reset();
      try {
        const res = await createRun({
          file_path: bot.workflowPath,
          // Cloud: the server resolves the bundle (source + skills) off the
          // pod FS by id, so Nexie launches without uploading bytes.
          bot_id: bot.id,
          vars,
          // Cloud repo scope: the runner clones the sidebar's active
          // repo so workspace_dir resolves to real code (and board
          // writes carry the repo tag). Without it Nexie only sees the
          // board — the launcher says so before launch.
          ...(repoScopeEnabled && activeRepo?.clone_url
            ? {
                repo_url: activeRepo.clone_url,
                connection_id: activeRepo.connection_id,
              }
            : {}),
          // The chat session was the one launch surface that could not
          // retarget its model — the per-run override mechanism already
          // existed and was generic, it just was not wired here. The helper
          // targets agent nodes only: the operator changes the answering
          // model while explicit judges keep their independent family.
          // Undefined when nothing is chosen, so the bot's DSL defaults apply
          // untouched.
          model_overrides: modelPrefOverrides(modelChoice?.()),
        });
        rememberSessionRunId(bot.id, scopeKey, res.run_id);
        // Pin the store's runId early so loadEventHistoryIfMissing's
        // `state.runId !== runId` guard doesn't drop the fetched batch
        // after its await — same trick the auto-attach branch uses.
        store.getState().setRunId(res.run_id);
        setRunId(res.run_id);
        // Seed the store with the freshly-created run's snapshot AND
        // any events the runtime persisted between createRun and now.
        // Without the second call, the WS subscribes at snap.last_seq+1
        // and silently misses everything up to that point — typically
        // run_started + the first node_started, which leaves the
        // transcript blank until propose_roadmap fires.
        try {
          // Retry-on-404: the launch API returns before the engine
          // goroutine flushed run.json, so an immediate getRun can 404
          // into this ignore-catch and skip the seeding entirely. The
          // hook-lifetime signal stops the retry loop on unmount (the
          // AbortError lands in the ignore-catch below).
          const snap = await getRunWithRetry(res.run_id, {
            signal: lifetimeAbortRef.current?.signal,
          });
          applySnapshot(snap);
          await loadEventHistoryIfMissing(res.run_id);
        } catch {
          // ignore; the WS will deliver the snapshot.
        }
      } catch (e) {
        setErrorMessage(toMessage(e));
        setStatus("idle");
        // Every composer destination rejects on failure so its caller keeps
        // the operator's draft and attached references. Swallowing only the
        // first-launch failure made the most expensive send path erase both.
        throw e;
      }
    },
    // Setters and the lifetime ref are stable (useState setters / a
    // ref box) — same dep list the pre-split hook used.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [
      bot.workflowPath,
      bot.id,
      scopeKey,
      repoScopeEnabled,
      activeRepo,
      modelChoice,
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
    forgetSessionRunId(bot.id, scopeKey);
    store.getState().reset();
    setRunId(null);
    setBusyMessageId(null);
    setErrorMessage(null);
    // setStatus("idle") happens automatically via the runId-null effect
    // in the orchestrating hook (watches runId for null → resets to idle).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [bot.id, scopeKey]);

  return { launch, newSession, lastVarsRef };
}
