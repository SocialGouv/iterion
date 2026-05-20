import { useCallback } from "react";
import { Link } from "wouter";
import { ExternalLinkIcon } from "@radix-ui/react-icons";

import {
  DEFAULT_WHATS_NEXT_BOT_ID,
  getFirstClassBot,
} from "@/lib/whats-next/firstClassBots";
import { useWhatsNextSession } from "@/lib/whats-next/useWhatsNextSession";

import AgentChatbox from "@/components/shared/AgentChatbox";
import ChatTranscript from "./ChatTranscript";
import PreFlightPanel from "./PreFlightPanel";
import SessionLauncher from "./SessionLauncher";

// WhatsNextView is the /whats-next route. It owns one whats-next session at a
// time via the useWhatsNextSession hook: the launcher creates the run,
// the transcript reads from the run store, and human turns are
// submitted via the hook's submitHumanAnswer.

export default function WhatsNextView() {
  const bot = getFirstClassBot(DEFAULT_WHATS_NEXT_BOT_ID);
  // Hooks must be called unconditionally — pass a dummy bot if the
  // lookup miss happens (in practice it can't since DEFAULT_WHATS_NEXT_BOT_ID
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
  //   outcome = { text, approved?, formAnswer? }
  //
  // When a node declares a rich `form:` in nodeMap, the FormAnswer's
  // question ids ARE the answer keys, so we forward verbatim.
  // Otherwise we look up textField/approvedField to build the
  // answers object from the legacy text + actions UI:
  //   ask_priorities → { context: text }
  //   human_review   → { feedback: text, approved: bool }
  const onHumanSubmit = useCallback(
    (
      messageId: string,
      outcome: {
        text: string;
        approved?: boolean;
        formAnswer?: Record<string, string | string[]>;
      },
    ) => {
      if (!bot) return;
      const m = session.messages.find((x) => x.id === messageId);
      if (!m || m.kind !== "human-question") return;
      const entry = bot.nodeMap[m.nodeId];
      if (!entry) return;
      if (outcome.formAnswer) {
        void session.submitHumanAnswer(messageId, outcome.formAnswer);
        return;
      }
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
      <div className="h-full grid place-items-center text-fg-muted">
        No first-class bot registered.
      </div>
    );
  }

  const inSession = session.status !== "idle";

  return (
    <div className="h-full flex flex-col overflow-hidden">
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
                bot={bot}
                onHumanSubmit={onHumanSubmit}
                busyMessageId={session.busyMessageId}
              />
            )}
            {session.errorMessage && (
              <div className="border-t border-danger/40 bg-danger-soft px-4 py-2 text-[12px] text-danger-fg">
                {session.errorMessage}
              </div>
            )}
            {session.runId &&
              session.runStatus !== "finished" &&
              session.runStatus !== "failed" &&
              session.runStatus !== "cancelled" && (
                <AgentChatbox runId={session.runId} />
              )}
          </div>
        )}
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
        {session.status === "ended" && (
          <button
            type="button"
            onClick={session.newSession}
            className="text-[11px] text-accent hover:underline cursor-pointer"
          >
            New session
          </button>
        )}
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
