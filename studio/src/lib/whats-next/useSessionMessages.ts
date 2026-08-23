// Message flow for the whats-next session: fold the run's event stream
// into the chat transcript, track the latest pending human question,
// and paper over the rare pause-window event gap with a one-shot
// refetch. Pure derivation lives in messagesFromEvents; this hook is
// the React glue around it.

import { useEffect, useMemo, useRef } from "react";

import {
  loadEvents,
  type RunEvent,
  type RunSnapshot,
  type RunStatus,
} from "@/api/runs";
import type { FirstClassBot } from "@/lib/whats-next/firstClassBots";
import type { WhatsNextMessage } from "@/lib/whats-next/messages";
import { useRunStoreInstance, type PendingHumanInput } from "@/store/run";

import {
  messagesFromEventsCached,
  type MessagesFoldCache,
} from "./messagesFromEvents";

export function useSessionMessages(opts: {
  bot: FirstClassBot;
  runId: string | null;
  runStatus: RunStatus | null;
  events: RunEvent[];
  snapshot: RunSnapshot | null;
  pendingHuman: PendingHumanInput | null;
}): WhatsNextMessage[] {
  const { bot, runId, runStatus, events, snapshot, pendingHuman } = opts;
  // The store this session lives in — the assistant's isolated one when
  // mounted under AssistantProvider, the module default otherwise.
  // Imperative reads MUST go through it: reaching for the module-default
  // `runStore` façade here would split the session's state (reads via
  // the provider, writes to another store).
  const store = useRunStoreInstance();

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
    const withSeed = out.slice();

    // The operator's OPENING message is not an event — it rides in as a launch
    // VAR (the bot's `chat.seed_var`), so a transcript folded from events
    // alone starts at the assistant's reply and the question it answers is
    // nowhere on screen. Read it back off the run's persisted inputs rather
    // than from the launch call, so a reattached session (reload, another
    // tab, days later) shows it too.
    const seedVar = bot.seedVar;
    const seedRaw = seedVar ? snapshot?.run?.inputs?.[seedVar] : undefined;
    const seed = typeof seedRaw === "string" ? seedRaw.trim() : "";
    if (seed) {
      withSeed.unshift({
        kind: "user-message",
        id: "seed",
        text: seed,
        // `consumed`, not `delivered`: the run READ this one — it is the launch
        // var the first node ran on. Claiming an in-flight state it never went
        // through is what made the opening message render unlike the rest.
        status: "consumed",
      });
    }
    return withSeed;
  }, [bot, events, snapshot]);

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

  // Human-gate lag mitigation (belt-and-braces). The pending question
  // FORM is derived from the `human_input_requested` EVENT via the
  // transcript fold — but the run's paused status can reach us through
  // a snapshot alone (REST getRun refresh, or the WS "snapshot"
  // envelope) when a WS disconnect/reconnect races the pause window and
  // the event never arrives. (Broker subscribers survive a pause since
  // 45bba653e, so this is a rare disconnect race, not the normal path.)
  // rehydratePendingHumanInput then fills the store's pendingHumanInput
  // from the checkpoint, but the fold has no event → no question message
  // → the form lags until a reload refetches history. When we observe
  // that inconsistent state, pull the event tail once per pause
  // transition (the ref resets whenever the run leaves
  // paused_waiting_human, and stays set otherwise, so a late backend
  // flush can't trigger a refetch loop).
  // We can't route through loadEventHistoryIfMissing here: it dedupes
  // via historyFetchedForRun, which is already stamped for this run.
  const pauseRefetchedForRef = useRef<string | null>(null);
  useEffect(() => {
    if (runStatus !== "paused_waiting_human") {
      pauseRefetchedForRef.current = null;
      return;
    }
    if (!runId) return;
    // Once-per-pause guard first: it's O(1) and satisfied in the steady
    // state, so the O(n) transcript scan below only runs until the one
    // refetch this pause is allowed has been issued.
    if (pauseRefetchedForRef.current === runId) return;
    const hasPendingQuestion = messages.some(
      (m) => m.kind === "human-question" && m.status === "pending",
    );
    if (hasPendingQuestion) return;
    pauseRefetchedForRef.current = runId;
    const stored = store.getState().events;
    const tail = stored[stored.length - 1];
    const fromSeq = tail ? tail.seq + 1 : 0;
    loadEvents(runId, fromSeq)
      .then((evts) => {
        if (store.getState().runId !== runId) return;
        if (evts.length > 0) store.getState().applyEventsBatch(evts);
      })
      .catch((err) => {
        // One shot per pause by design (loop guard); on failure the
        // WS/resume resync paths remain the fallback. Surface it in
        // devtools so silent 401/5xx don't go unnoticed.
        console.warn("[whats-next] pause-gate event refetch failed", err);
      });
  }, [runId, runStatus, messages]);

  return messages;
}
