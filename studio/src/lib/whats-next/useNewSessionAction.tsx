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

import { useCallback, useEffect, useRef, useState } from "react";

import { useConfirm } from "@/hooks/useConfirm";
import {
  cancelThenDispose,
  shouldConfirmRunDisposal,
} from "@/lib/chatDock/conversationDisposal";
import { errorMessage } from "@/lib/errorHints";
import type { useWhatsNextSession } from "@/lib/whats-next/useWhatsNextSession";
import { useUIStore } from "@/store/ui";

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
  const busyRef = useRef(false);
  const retryRef = useRef<() => void>(() => {});
  const { confirm, dialog } = useConfirm();
  const addToast = useUIStore((state) => state.addToast);

  const start = useCallback(async () => {
    if (busyRef.current) return;
    busyRef.current = true;
    setBusy(true);
    try {
      if (
        shouldConfirmRunDisposal(session.runId, session.runStatus) &&
        !(await confirm({
          title: `Cancel running ${bot.label} session?`,
          message: `Cancelling ends the conversation — ${bot.label} forgets everything you discussed. The transcript stays readable in the run console, but the next session starts with no memory of it.`,
          confirmLabel: "Cancel and start new",
          confirmVariant: "danger",
        }))
      ) {
        return;
      }
      await cancelThenDispose({
        runId: session.runId,
        dispose: session.newSession,
      });
    } catch (error) {
      addToast(`Could not start a new assistant session: ${errorMessage(error)}`, "error", {
        persistent: true,
        action: { label: "Retry", onClick: () => retryRef.current() },
      });
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  }, [session, confirm, bot.label, addToast]);
  useEffect(() => {
    retryRef.current = () => void start();
  }, [start]);

  // Available across every run state so the operator can always escape —
  // gating it on "ended" used to trap them inside paused or failed_resumable
  // sessions. Hidden only when there is nothing yet to reset.
  const available = session.runId !== null && session.status !== "launching";

  return { start, busy, available, dialog };
}
