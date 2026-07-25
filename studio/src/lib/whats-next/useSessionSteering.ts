// Steering actions for an attached whats-next session: answer the
// pending human gate (the chat reply path) and bare-resume a
// failed_resumable / cancelled run. Both share the same post-resume
// recovery choreography: optimistic status flip, WS redial, and a
// belt-and-braces snapshot refresh shortly after.

import { useCallback, useEffect, useRef } from "react";

// Aliased: the orchestrating hook binds a local `errorMessage` field.
import { errorMessage as toMessage } from "@/lib/errorHints";

import { getRun, resumeRun } from "@/api/runs";
import { runStore, useRunStore } from "@/store/run";

import type { WhatsNextStatus } from "./sessionStatus";

export interface SessionSteering {
  submitHumanAnswer: (
    messageId: string,
    answers: Record<string, unknown>,
  ) => Promise<void>;
  resume: () => Promise<void>;
}

export function useSessionSteering(opts: {
  runId: string | null;
  setStatus: (status: WhatsNextStatus) => void;
  setBusyMessageId: (id: string | null) => void;
  setErrorMessage: (msg: string | null) => void;
}): SessionSteering {
  const { runId, setStatus, setBusyMessageId, setErrorMessage } = opts;

  const setRunStatus = useRunStore((s) => s.setRunStatus);
  const requestWsReconnect = useRunStore((s) => s.requestWsReconnect);
  const applySnapshot = useRunStore((s) => s.applySnapshot);

  // Belt-and-braces snapshot refresh scheduled after a resume. Held in
  // a ref so we can cancel a pending timer when the hook unmounts or
  // when a new submit supersedes the previous one — without this, a
  // fast WhatsNext-to-WhatsNext navigation would let an old timer
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

  // Refresh the snapshot ~600ms after a resume so a short-lived run
  // that finishes before the WS redial still surfaces a final state.
  // We capture the target runId so a late-firing timer can't apply a
  // snapshot for a stale session (e.g. after WhatsNext-to-WhatsNext
  // navigation within 600ms), and we cancel any previous pending timer
  // so only the most recent submit's refresh wins.
  const scheduleSnapshotRefresh = useCallback(
    (targetRunId: string) => {
      if (refreshTimerRef.current !== null) {
        window.clearTimeout(refreshTimerRef.current);
      }
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
    },
    [applySnapshot],
  );

  const submitHumanAnswer = useCallback(
    async (messageId: string, answers: Record<string, unknown>) => {
      if (!runId) {
        console.warn("[whats-next] submitHumanAnswer: no runId, aborting");
        return;
      }
      setErrorMessage(null);
      setBusyMessageId(messageId);
      setStatus("submitting");
      try {
        // `force: true` is intentional. After any bot edit (the
        // operator iterates on prompts mid-session) the workflow
        // hash changes; the engine's checkWorkflowHash silently
        // rejects the resume — but the HTTP layer returns 202
        // before the goroutine validates, so the SPA sees a fake
        // success while the engine sits idle. Resume from inside
        // /whats-next is unambiguously "retry with my edits", so we
        // pass force every time. The /runs/<id> console retains the
        // explicit toggle for the rare hash-pinned case.
        await resumeRun(runId, { answers, force: true });
        setRunStatus("running");
        // Re-dial the WS so the resumed engine's events reach us even
        // if the connection silently dropped across the pause window.
        // Belt-and-braces: subscribers survive a pause since 45bba653e,
        // so this only covers disconnect/reconnect races. Same trick
        // the generic HumanPromptForm uses.
        requestWsReconnect();
        scheduleSnapshotRefresh(runId);
        setStatus("active");
      } catch (e) {
        if (typeof console !== "undefined") {
          console.error("[whats-next] submitHumanAnswer error", e);
        }
        setErrorMessage(toMessage(e));
        setStatus("active");
        // Rethrow so an awaiting composer keeps the operator's typed
        // reply (AgentChatboxInline only clears the draft when onSend
        // resolves).
        throw e;
      } finally {
        setBusyMessageId(null);
      }
    },
    // Setters are stable useState setters — same dep list the pre-split
    // hook used, plus the extracted refresh scheduler.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [runId, setRunStatus, requestWsReconnect, scheduleSnapshotRefresh],
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
      scheduleSnapshotRefresh(runId);
      setStatus("active");
    } catch (e) {
      setErrorMessage(toMessage(e));
      setStatus("ended");
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [runId, setRunStatus, requestWsReconnect, scheduleSnapshotRefresh]);

  return { submitHumanAnswer, resume };
}
