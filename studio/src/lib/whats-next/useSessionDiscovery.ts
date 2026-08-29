// Auto-attach discovery for the whats-next session. On mount (and on
// scope change / manual retry), decide which run — if any — the
// session should attach to, hydrate the store, and hand the runId back
// to the orchestrating hook.

import { useCallback, useEffect, useRef, useState } from "react";

import { useRunStoreInstance } from "@/store/run";

// Aliased: the orchestrating hook binds a local `errorMessage` field.
import { errorMessage as toMessage } from "@/lib/errorHints";

import type { ForgeTeamRepo } from "@/api/forgeConnections";
import type { FirstClassBot } from "@/lib/whats-next/firstClassBots";

import type { WhatsNextStatus } from "./sessionStatus";
import {
  forgetSessionRunId,
  recallSessionRunId,
} from "./sessionStorage";
import { attachSessionRun, findLiveRunForBot } from "./startupDiscovery";

export interface SessionDiscovery {
  // Startup discovery failure (couldn't list runs to find a live
  // session). Surfaced so the launcher can warn: launching blind may
  // start a second parallel session. Null when discovery succeeded.
  discoveryError: string | null;
  // Re-run the startup discovery after a failure.
  retryDiscovery: () => void;
}

export function useSessionDiscovery(opts: {
  bot: FirstClassBot;
  scopeKey: string | null;
  repoScopeEnabled: boolean;
  overview: boolean;
  activeRepo: ForgeTeamRepo | null;
  // Called with the attached run's id once the store is hydrated.
  onAttached: (runId: string) => void;
  setStatus: (status: WhatsNextStatus) => void;
  // Should this session look for an existing run to attach to?
  //
  // Discovery is keyed on (bot, scope), so with several conversations open on
  // the SAME bot it hands every one of them the same run — clicking "new
  // conversation" showed the old one. A conversation the operator just opened
  // must therefore start empty and launch its own; only a session that IS the
  // continuation of an earlier one should attach.
  discover?: boolean;
  // Attach to THIS run, skipping the bot-scoped lookup entirely.
  //
  // The lookup answers "the latest live run for this bot", which is the wrong
  // question once several conversations share a bot: whichever one remounted
  // last would take another's run. A conversation that has launched knows its
  // own run id and must use it.
  attachRunId?: string | null;
}): SessionDiscovery {
  const { bot, scopeKey, repoScopeEnabled, overview, activeRepo } = opts;
  const discover = opts.discover ?? true;
  const attachRunId = opts.attachRunId ?? null;
  const { onAttached, setStatus } = opts;
  // The store this session lives in — the assistant's isolated one when
  // mounted under AssistantProvider, the module default otherwise. Stable
  // for the hook's lifetime, so it stays out of the effect deps.
  const store = useRunStoreInstance();

  // Auto-attach: on mount, if we remembered a runId for this bot+scope,
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
  const [discoveryError, setDiscoveryError] = useState<string | null>(null);
  // Bumping the nonce re-arms the discovery effect (retry after a
  // listRuns failure).
  const [discoveryNonce, setDiscoveryNonce] = useState(0);
  const attachAttemptedRef = useRef(false);
  useEffect(() => {
    if (attachAttemptedRef.current) return;
    if (!bot.id) return;
    // In cloud, wait for the repo scope to resolve before discovering —
    // the scope key participates in both storage and filtering.
    attachAttemptedRef.current = true;
    const controller = new AbortController();
    let cancelled = false;

    const attachTo = async (runIdToAttach: string) => {
      const attached = await attachSessionRun({
        store,
        runId: runIdToAttach,
        botId: bot.id,
        scopeKey,
        signal: controller.signal,
        isCancelled: () => cancelled,
      });
      if (attached && !cancelled) onAttached(runIdToAttach);
      return attached && !cancelled;
    };

    const teardown = () => {
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

    if (attachRunId) {
      attachAttemptedRef.current = true;
      void (async () => {
        try {
          await attachTo(attachRunId);
        } catch {
          // A run that is gone (pruned, cancelled elsewhere) leaves the
          // conversation on its empty state rather than borrowing another's.
        }
      })();
      return teardown;
    }
    if (!discover) {
      // Not an error state: this conversation is meant to be empty until the
      // operator says something.
      attachAttemptedRef.current = true;
      return teardown;
    }

    const remembered = recallSessionRunId(bot.id, scopeKey);
    setStatus("launching");
    // Discovery decides between three signals, in order:
    //   1. A live (non-terminal) run for this bot — even if localStorage
    //      remembers an older terminal session, the operator landing on
    //      /whats-next while a paused/running session exists ALMOST
    //      ALWAYS wants the live one (they relaunched from /runs, the
    //      CLI, or another tab). Continuity is good; surfacing a stale
    //      "Ended · cancelled" session while a live one waits at a
    //      human gate is much worse.
    //   2. The remembered run, terminal or not, for continuity ("show
    //      me what I just did").
    //   3. Idle, so the launcher renders.
    const repoFilter =
      repoScopeEnabled && !overview && activeRepo
        ? activeRepo.repo_full_name
        : undefined;
    const startup = (async () => {
      try {
        const live = await findLiveRunForBot(bot.id, repoFilter);
        if (cancelled) return;
        setDiscoveryError(null);
        if (live) {
          if (await attachTo(live)) return;
        }
        if (remembered) {
          try {
            if (await attachTo(remembered)) return;
            // The snapshot exists but belongs to another bot. Retire the
            // stale pointer and fall through to the launcher.
            forgetSessionRunId(bot.id, scopeKey);
          } catch (err) {
            if (
              controller.signal.aborted ||
              (err as Error)?.name === "AbortError"
            ) {
              return;
            }
            // Remembered run no longer exists (rotated, store wiped).
            // Drop the memory; we have no live run either, so fall
            // through to idle.
            forgetSessionRunId(bot.id, scopeKey);
          }
        }
        setStatus("idle");
      } catch (err) {
        if (
          controller.signal.aborted ||
          (err as Error)?.name === "AbortError"
        ) {
          return;
        }
        // A failed discovery must be VISIBLE: a live session may exist
        // that we couldn't see, and launching blind doubles the spend.
        // The launcher renders this with a Retry.
        setDiscoveryError(toMessage(err));
        setStatus("idle");
      }
    })();

    void startup;
    return teardown;
    // scopeKey folds in (team, active repo) — a repo switch re-runs the
    // discovery for the new scope; discoveryNonce is the manual retry.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [bot.id, scopeKey, discoveryNonce, discover, attachRunId]);

  const retryDiscovery = useCallback(() => {
    setDiscoveryError(null);
    setDiscoveryNonce((n) => n + 1);
  }, []);

  return { discoveryError, retryDiscovery };
}
