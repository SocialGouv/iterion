import { useCallback } from "react";
import { Link } from "wouter";
import { ExternalLinkIcon } from "@radix-ui/react-icons";

import AppHeader from "@/components/shared/AppHeader";
import {
  DEFAULT_PILOTE_BOT_ID,
  getFirstClassBot,
} from "@/lib/pilote/firstClassBots";
import { useWhatsNextSession } from "@/lib/pilote/useWhatsNextSession";

import ChatTranscript from "./ChatTranscript";
import PreFlightPanel from "./PreFlightPanel";
import SessionLauncher from "./SessionLauncher";

// PiloteView is the /pilote route. It owns one whats-next session at a
// time via the useWhatsNextSession hook: the launcher creates the run,
// the transcript reads from the run store, and human turns are
// submitted via the hook's submitHumanAnswer.

export default function PiloteView() {
  const bot = getFirstClassBot(DEFAULT_PILOTE_BOT_ID);
  // Hooks must be called unconditionally — pass a dummy bot if the
  // lookup miss happens (in practice it can't since DEFAULT_PILOTE_BOT_ID
  // is a const key, but the early-return branch needs valid hook order).
  const session = useWhatsNextSession(
    bot ?? {
      id: "",
      label: "",
      description: "",
      workflowPath: "",
      launcherVars: [],
      nodeMap: {},
    },
  );

  // submit is shaped to match HumanChatTurn's contract:
  //   outcome = { text, approved? }
  // We look up the message's nodeId in the bot.nodeMap to learn the
  // schema field names. ask_priorities expects `context`,
  // human_review expects `feedback` + `approved`. Bot authors who
  // want a different schema add the right textField/approvedField
  // entries to nodeMap.
  const onHumanSubmit = useCallback(
    (messageId: string, outcome: { text: string; approved?: boolean }) => {
      if (!bot) return;
      const m = session.messages.find((x) => x.id === messageId);
      if (!m || m.kind !== "human-question") return;
      const entry = bot.nodeMap[m.nodeId];
      if (!entry) return;
      const answers: Record<string, unknown> = {};
      if (entry.textField) {
        answers[entry.textField] = outcome.text;
      }
      if (entry.approvedField && outcome.approved !== undefined) {
        answers[entry.approvedField] = outcome.approved;
      }
      void session.submitHumanAnswer(messageId, answers);
    },
    [bot, session],
  );

  if (!bot) {
    return (
      <div className="h-full flex flex-col bg-surface-0">
        <AppHeader active="pilote" />
        <main className="flex-1 grid place-items-center text-fg-muted">
          No first-class bot registered.
        </main>
      </div>
    );
  }

  const inSession = session.status !== "idle";

  return (
    <div className="h-full flex flex-col bg-surface-0 text-fg-default overflow-hidden">
      <AppHeader active="pilote" />

      <main className="flex-1 flex flex-col overflow-hidden">
        {!inSession ? (
          <SessionLauncher
            bot={bot}
            onLaunch={(vars) => void session.launch(vars)}
            busy={session.status === "launching"}
            errorMessage={session.errorMessage}
          />
        ) : (
          <div className="flex-1 flex flex-col max-w-3xl w-full mx-auto overflow-hidden">
            <SessionHeader bot={bot} session={session} />
            {session.messages.length === 0 ? (
              <div className="flex-1 overflow-y-auto">
                <PreFlightPanel
                  runId={session.runId}
                  status={session.status}
                  runStatus={session.runStatus}
                  rawEventCount={session.rawEventCount}
                />
              </div>
            ) : (
              <ChatTranscript
                messages={session.messages}
                onHumanSubmit={onHumanSubmit}
                busyMessageId={session.busyMessageId}
              />
            )}
            {session.errorMessage && (
              <div className="border-t border-danger/40 bg-danger-soft px-4 py-2 text-[12px] text-danger-fg">
                {session.errorMessage}
              </div>
            )}
          </div>
        )}
      </main>
    </div>
  );
}

function SessionHeader({
  bot,
  session,
}: {
  bot: { label: string };
  session: ReturnType<typeof useWhatsNextSession>;
}) {
  return (
    <div className="px-4 py-3 border-b border-border-subtle flex items-baseline justify-between gap-3">
      <h2 className="text-[13px] font-semibold text-fg-default">
        {bot.label}
        {session.runId && (
          <span className="ml-2 text-[10px] text-fg-subtle font-mono font-normal">
            {session.runId}
          </span>
        )}
      </h2>
      <div className="flex items-baseline gap-3">
        {session.runId && (
          <Link
            href={`/runs/${encodeURIComponent(session.runId)}`}
            className="inline-flex items-center gap-1 text-[11px] text-accent hover:underline"
          >
            <ExternalLinkIcon className="w-3 h-3" />
            console
          </Link>
        )}
        <div className="text-[10px] uppercase tracking-wide text-fg-subtle">
          {humanStatus(session.status, session.runStatus)}
        </div>
      </div>
    </div>
  );
}

function humanStatus(
  hi: ReturnType<typeof useWhatsNextSession>["status"],
  raw: ReturnType<typeof useWhatsNextSession>["runStatus"],
): string {
  if (hi === "launching") return "launching…";
  if (hi === "submitting") return "submitting…";
  if (hi === "ended") return `ended (${raw ?? "unknown"})`;
  if (raw === "paused_waiting_human") return "waiting for you";
  return "running";
}
