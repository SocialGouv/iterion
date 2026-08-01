import {
  AssistantStoreScope,
  useAssistant,
} from "@/components/ChatDock/AssistantProvider";
import { ASK_USER_RESPONSE_KEY } from "@/lib/askUserOptions";
import type { FirstClassBot } from "@/lib/whats-next/firstClassBots";
import { useAssistantComposer } from "@/lib/whats-next/useAssistantComposer";
import type { UseWhatsNextSession } from "@/lib/whats-next/useWhatsNextSession";

import AgentChatbox from "@/components/shared/AgentChatbox";
import { Button } from "@/components/ui/Button";
import ChatTranscript from "./ChatTranscript";
import PreFlightPanel from "./PreFlightPanel";
import SessionLauncher from "./SessionLauncher";
import WatchPanel from "./WatchPanel";
import ResumeFooter from "./whatsNextView/ResumeFooter";
import SessionHeader from "./whatsNextView/SessionHeader";
import { composerPlaceholder } from "./whatsNextView/composerPlaceholder";

// WhatsNextView is the /whats-next route — Nexie's chat, full width.
//
// It no longer OWNS the session: the session is mounted once above the
// route tree by AssistantProvider, so the same conversation is reachable
// from the shell-level dock on every other route and navigating here
// (or away) neither restarts it nor loses the transcript. This route is
// now one of two views onto that session, and the roomier one.
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
  const assistant = useAssistant();
  if (!assistant?.bot) {
    return (
      <div className="h-full grid place-items-center text-fg-muted">
        No first-class bot registered.
      </div>
    );
  }
  // The transcript + composer read the run store; re-enter the
  // assistant's isolated one (AssistantProvider hands the default store
  // to the route tree).
  return (
    <AssistantStoreScope>
      <WhatsNextConversation bot={assistant.bot} session={assistant.session} />
    </AssistantStoreScope>
  );
}

function WhatsNextConversation({
  bot,
  session,
}: {
  bot: FirstClassBot;
  session: UseWhatsNextSession;
}) {
  // The composer routing is shared with the shell-level dock — see
  // useAssistantComposer for the four destinations a message can take.
  const {
    pendingHumanQuestion,
    pendingIsAskUser,
    options,
    quickReplies,
    allowFreeText,
    busyPending,
    submitPending,
    onComposerSend,
  } = useAssistantComposer({ bot, session });

  const inSession = session.status !== "idle";

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
