import { useCallback } from "react";

import {
  DEFAULT_WHATS_NEXT_BOT_ID,
  getFirstClassBot,
} from "@/lib/whats-next/firstClassBots";
import { useWhatsNextSession } from "@/lib/whats-next/useWhatsNextSession";
import {
  askUserAllowsFreeText,
  askUserOptions,
  ASK_USER_RESPONSE_KEY,
} from "@/lib/askUserOptions";

import { queueMessage } from "@/api/queueMessages";
import AgentChatbox from "@/components/shared/AgentChatbox";
import { Button } from "@/components/ui/Button";
import ChatTranscript from "./ChatTranscript";
import PreFlightPanel from "./PreFlightPanel";
import SessionLauncher from "./SessionLauncher";
import WatchPanel from "./WatchPanel";
import ResumeFooter from "./whatsNextView/ResumeFooter";
import SessionHeader from "./whatsNextView/SessionHeader";
import { composerPlaceholder } from "./whatsNextView/composerPlaceholder";

// WhatsNextView is the /whats-next route — Nexie's chat. It owns one
// whats-next session at a time via the useWhatsNextSession hook.
//
// v2: ONE always-on composer is the single input surface. Depending on
// the session state, a submitted message:
//   - answers the pending `chat` pause (Nexie's turn-end question),
//   - answers a pending mid-turn ask_user pause,
//   - queues into the running agent's inbox,
//   - or re-seeds a fresh session (vars[seedVar] = the text).
// Clickable chips ride above the composer: Nexie's quick_replies on a
// chat pause, the structured options on an ask_user pause.

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

  const pendingHumanQuestion = session.messages.find(
    (m): m is Extract<typeof m, { kind: "human-question" }> =>
      m.kind === "human-question" && m.status === "pending",
  );

  // A pending ask_user pause (mid-turn agent question) answers with a
  // single string under ask_user_response; the chat node's pause
  // answers with {message}. Both flow through the same composer.
  const pendingIsAskUser =
    !!pendingHumanQuestion?.questions &&
    ASK_USER_RESPONSE_KEY in pendingHumanQuestion.questions;
  const pendingAnswerKey = pendingIsAskUser
    ? ASK_USER_RESPONSE_KEY
    : bot?.nodeMap[pendingHumanQuestion?.nodeId ?? ""]?.textField ?? "message";

  // Clickable chips: ask_user structured options win; otherwise the
  // chat turn's quick_replies (Nexie's suggested next messages).
  const options = pendingIsAskUser
    ? askUserOptions(pendingHumanQuestion?.questions)
    : [];
  const allowFreeText = pendingIsAskUser
    ? askUserAllowsFreeText(pendingHumanQuestion?.questions)
    : true;
  const quickReplies: string[] = !pendingIsAskUser
    ? readQuickReplies(pendingHumanQuestion?.questions)
    : [];

  // Awaited (not fire-and-forget) so the composer's draft survives a
  // failed submit: AgentChatboxInline only clears the text when onSend
  // resolves, and submitHumanAnswer rethrows on failure.
  const submitPending = useCallback(
    async (value: string) => {
      if (!pendingHumanQuestion) return;
      await session.submitHumanAnswer(pendingHumanQuestion.id, {
        [pendingAnswerKey]: value,
      });
    },
    [pendingHumanQuestion, pendingAnswerKey, session],
  );

  // The unified composer routing (see the file comment).
  const onComposerSend = useCallback(
    async (text: string, opts: { skills: string[] }) => {
      const trimmed = text.trim();
      if (trimmed === "") return;
      if (pendingHumanQuestion) {
        await submitPending(trimmed);
        return;
      }
      const status = session.runStatus;
      const closed =
        !session.runId ||
        status === "finished" ||
        status === "failed" ||
        status === "cancelled";
      if (closed) {
        const seedVar = bot?.seedVar ?? "initial_message";
        await session.launch({
          ...(session.lastVars ?? {}),
          [seedVar]: trimmed,
        });
        return;
      }
      // Not closed ⇒ runId is truthy (it's part of the `closed`
      // disjunction above), so the run is live: inject into its inbox.
      await queueMessage(session.runId!, trimmed, { skills: opts.skills });
    },
    [pendingHumanQuestion, submitPending, session, bot?.seedVar],
  );

  if (!bot) {
    return (
      <div className="h-full grid place-items-center text-fg-muted">
        No first-class bot registered.
      </div>
    );
  }

  const inSession = session.status !== "idle";
  const busyPending =
    !!pendingHumanQuestion &&
    session.busyMessageId === pendingHumanQuestion.id;

  return (
    <div className="h-full flex flex-col overflow-hidden">
        {!inSession ? (
          <SessionLauncher
            bot={bot}
            onLaunch={({ vars }) => {
              void session.launch(vars);
            }}
            busy={session.status === "launching"}
            errorMessage={session.errorMessage}
            discoveryError={session.discoveryError}
            onRetryDiscovery={session.retryDiscovery}
            launchRepo={session.launchRepo}
          />
        ) : (
          <div className="flex-1 flex flex-col max-w-3xl w-full mx-auto overflow-hidden">
            <SessionHeader bot={bot} session={session} />
            <WatchPanel runId={session.runId} />
            {session.messages.length === 0 ? (
              <div className="flex-1 overflow-y-auto">
                <PreFlightPanel
                  runId={session.runId}
                  runStatus={session.runStatus}
                />
              </div>
            ) : (
              <ChatTranscript
                messages={session.messages}
                bot={bot}
                busyMessageId={session.busyMessageId}
                // The pending turn stays inline as Nexie's bubble; its
                // input lives in the unified composer below. The
                // onHumanSubmit fallback only fires for a pending card
                // the composer somehow doesn't own (defensive).
                composerHandlesId={pendingHumanQuestion?.id}
                onHumanSubmit={(messageId, outcome) => {
                  const m = session.messages.find((x) => x.id === messageId);
                  if (!m || m.kind !== "human-question") return;
                  const isAsk =
                    !!m.questions && ASK_USER_RESPONSE_KEY in m.questions;
                  const key = isAsk
                    ? ASK_USER_RESPONSE_KEY
                    : bot.nodeMap[m.nodeId]?.textField ?? "message";
                  void session
                    .submitHumanAnswer(messageId, { [key]: outcome.text })
                    .catch(() => {});
                }}
              />
            )}
            {session.errorMessage && (
              <div className="border-t border-danger/40 bg-danger-soft px-4 py-2 text-body text-danger-fg">
                {session.errorMessage}
              </div>
            )}
            {session.runId &&
            (session.runStatus === "failed_resumable" ||
              session.runStatus === "cancelled") ? (
              // Terminal-but-resumable wins over the composer: submitting
              // a pause answer against a dead run misleads. Resume first;
              // the pending question is re-shown by the new engine pass.
              <ResumeFooter
                runStatus={session.runStatus}
                busy={session.status === "submitting"}
                onResume={() => void session.resume()}
              />
            ) : (
              session.runId && (
                <div className="border-t border-border-subtle">
                  {(options.length > 0 || quickReplies.length > 0) && (
                    <div className="flex flex-wrap gap-2 px-4 pt-3">
                      {/* Chip failures already surface via the session's
                          errorMessage banner — swallow the rethrow that
                          exists for the composer's draft preservation. */}
                      {options.map((o) => (
                        <Button
                          key={o.id}
                          variant="secondary"
                          size="sm"
                          disabled={busyPending}
                          onClick={() => void submitPending(o.id).catch(() => {})}
                        >
                          {o.label}
                        </Button>
                      ))}
                      {quickReplies.map((q) => (
                        <Button
                          key={q}
                          variant="secondary"
                          size="sm"
                          disabled={busyPending}
                          onClick={() => void submitPending(q).catch(() => {})}
                        >
                          {q}
                        </Button>
                      ))}
                    </div>
                  )}
                  {/* ask_user with options may disallow free text — the
                      chips above are then the only input. */}
                  {(!pendingIsAskUser || allowFreeText || options.length === 0) && (
                    <AgentChatbox
                      runId={session.runId}
                      embedded
                      placeholder={composerPlaceholder(
                        session.runStatus,
                        !!pendingHumanQuestion,
                      )}
                      onSend={onComposerSend}
                    />
                  )}
                </div>
              )
            )}
          </div>
        )}
    </div>
  );
}

// readQuickReplies lifts Nexie's suggested next messages off the chat
// pause's questions payload (`quick_replies: json` on the turn output,
// mapped into the chat node's input). Tolerates absent / malformed
// payloads — chips are sugar, never load-bearing. A `json`-typed schema
// field can arrive as the literal TEXT of a JSON array (the LLM emits
// the array stringified) — parse that shape too.
function readQuickReplies(
  questions: Record<string, unknown> | undefined,
): string[] {
  let raw = questions?.quick_replies;
  if (typeof raw === "string" && raw.trim().startsWith("[")) {
    try {
      raw = JSON.parse(raw);
    } catch {
      return [];
    }
  }
  if (!Array.isArray(raw)) return [];
  return raw
    .filter((v): v is string => typeof v === "string" && v.trim().length > 0)
    .slice(0, 4);
}
