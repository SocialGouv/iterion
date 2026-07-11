import { useCallback, useState } from "react";
import { Link } from "wouter";
import { ExternalLinkIcon } from "@radix-ui/react-icons";

import { cancelRun } from "@/api/runs";
import { CopyButton } from "@/components/ui/CopyButton";
import { useConfirm } from "@/hooks/useConfirm";
import type { useWhatsNextSession } from "@/lib/whats-next/useWhatsNextSession";

import { humanStatus } from "./humanStatus";

export default function SessionHeader({
  bot,
  session,
}: {
  bot: { label: string };
  session: ReturnType<typeof useWhatsNextSession>;
}) {
  const [abandoning, setAbandoning] = useState(false);
  const { confirm, dialog } = useConfirm();
  // A session is "live" when it has an in-flight run that hasn't reached
  // a terminal state. Abandoning a live run must cancel it server-side
  // before resetting the UI — otherwise newSession just orphans the
  // engine goroutine, which keeps burning model spend until something
  // else (stall watchdog, process restart) tears it down.
  const isLive =
    session.runId !== null &&
    session.status !== "ended" &&
    session.status !== "idle";

  const onNewSession = useCallback(async () => {
    if (isLive) {
      const ok = await confirm({
        title: "Cancel running Nexie session?",
        message:
          "The current Nexie session is still running. Cancel it and start a new one?",
        confirmLabel: "Cancel and start new",
        confirmVariant: "danger",
      });
      if (!ok) return;
      setAbandoning(true);
      try {
        if (session.runId) {
          await cancelRun(session.runId);
        }
      } catch {
        // Surface but don't block: even if cancel races (e.g. the run
        // just finished), the reset below still lands the user on a
        // fresh launcher; the worst case is a quiescent orphan that
        // the existing stall sweep will reconcile.
      } finally {
        setAbandoning(false);
      }
    }
    session.newSession();
  }, [isLive, session, confirm]);

  // The button is hidden when there's nothing to reset (no runId yet,
  // pre-launch). Otherwise it stays available across every run state
  // so the operator can always escape — the prior behaviour gated it
  // on `status === "ended"`, which trapped them inside paused or
  // failed_resumable sessions.
  const showResetButton = session.runId !== null && session.status !== "launching";

  return (
    <div className="px-4 py-3 border-b border-border-subtle flex items-baseline justify-between gap-3">
      {dialog}
      <h2 className="text-label font-semibold text-fg-default inline-flex items-baseline gap-2">
        {bot.label}
        {session.runId && (
          <CopyButton
            value={session.runId}
            label="copy run id"
            className="font-normal"
          />
        )}
      </h2>
      <div className="flex items-baseline gap-3">
        {session.runId && (
          <Link
            href={`/runs/${encodeURIComponent(session.runId)}`}
            className="inline-flex items-center gap-1 text-micro text-accent-text hover:underline"
          >
            <ExternalLinkIcon className="w-3 h-3" />
            console
          </Link>
        )}
        <div className="text-caption uppercase tracking-wide text-fg-subtle">
          {humanStatus(session.status, session.runStatus)}
        </div>
        {showResetButton && (
          <button
            type="button"
            onClick={() => void onNewSession()}
            disabled={abandoning}
            className="text-micro text-accent-text hover:underline cursor-pointer disabled:opacity-50 disabled:cursor-wait"
            title={
              isLive
                ? "Cancel the current run and start a fresh Nexie session."
                : "Start fresh — the current run stays in the run list."
            }
          >
            {abandoning ? "Cancelling…" : isLive ? "Abandon & restart" : "New session"}
          </button>
        )}
      </div>
    </div>
  );
}

