// "Start a new conversation" — the chat equivalent of a CLI's `/new`.
//
// Shared by BOTH assistant surfaces on purpose. The route view had this for a
// long time and the dock did not, which stopped being survivable once a bot
// could live in the dock alone: its conversation had no way to end at all.
//
// One implementation because the dangerous half is easy to omit. Abandoning a
// LIVE session must cancel the run server-side before the UI resets —
// otherwise `newSession()` orphans the engine goroutine, which keeps burning
// model spend until a stall watchdog or a process restart tears it down. A
// second copy of this that forgot the cancel would be a spend leak nobody
// sees.

import { useCallback, useState } from "react";

import { cancelRun } from "@/api/runs";
import { useConfirm } from "@/hooks/useConfirm";
import type { useWhatsNextSession } from "@/lib/whats-next/useWhatsNextSession";

export interface NewSessionAction {
  /** Runs the confirm → cancel → reset sequence. */
  start: () => Promise<void>;
  /** True while the cancel request is in flight. */
  busy: boolean;
  /** False when there is nothing to reset (pre-launch, or mid-launch). */
  available: boolean;
  /** The confirm dialog element — render it inside the calling surface. */
  dialog: React.ReactNode;
}

export function useNewSessionAction({
  bot,
  session,
}: {
  bot: { label: string };
  session: ReturnType<typeof useWhatsNextSession>;
}): NewSessionAction {
  const [busy, setBusy] = useState(false);
  const { confirm, dialog } = useConfirm();

  // "Live" = an in-flight run that has not reached a terminal state.
  const isLive =
    session.runId !== null &&
    session.status !== "ended" &&
    session.status !== "idle";

  const start = useCallback(async () => {
    if (isLive) {
      const ok = await confirm({
        title: `Cancel running ${bot.label} session?`,
        message: `Cancelling ends the conversation — ${bot.label} forgets everything you discussed. The transcript stays readable in the run console, but the next session starts with no memory of it.`,
        confirmLabel: "Cancel and start new",
        confirmVariant: "danger",
      });
      if (!ok) return;
      setBusy(true);
      try {
        if (session.runId) await cancelRun(session.runId);
      } catch {
        // Surface but don't block: even if the cancel races (the run may
        // have just finished), the reset below still lands the operator on a
        // fresh launcher. Worst case is a quiescent orphan the stall sweep
        // reconciles.
      } finally {
        setBusy(false);
      }
    }
    session.newSession();
  }, [isLive, session, confirm, bot.label]);

  // Available across every run state so the operator can always escape —
  // gating it on "ended" used to trap them inside paused or failed_resumable
  // sessions. Hidden only when there is nothing yet to reset.
  const available = session.runId !== null && session.status !== "launching";

  return { start, busy, available, dialog };
}
