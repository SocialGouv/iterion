// The ubiquitous assistant dock.
//
// Mounted once at shell level (App.tsx, next to GlobalCommandPalette),
// so it is reachable from every authenticated route and survives
// navigation by construction — it is never inside the <Route> tree.
//
// It is a VIEW onto the session AssistantProvider owns, not an owner:
// /whats-next renders the same session full-width. Switching between the
// two keeps one run and one transcript.
//
// What it adds over the route view is implicit context — the reference
// for the page the operator is on, pinned visibly above the composer and
// carried as a one-line pointer on every message.

import { useCallback, useEffect, useRef } from "react";

import {
  AssistantStoreScope,
  useAssistantDock,
  useAssistantSession,
} from "@/components/ChatDock/AssistantProvider";
import ContextChip from "@/components/ChatDock/ContextChip";
import { ChatDockShell } from "@/components/ChatDock/ChatDockShell";
import AgentChatboxInline from "@/components/shared/AgentChatboxInline";
import { Button } from "@/components/ui/Button";
import ChatTranscript from "@/components/WhatsNext/ChatTranscript";
import { composerPlaceholder } from "@/components/WhatsNext/whatsNextView/composerPlaceholder";
import { withPageContext } from "@/lib/chatDock/contextMessage";
import type { DockState } from "@/lib/chatDock/dockState";
import { ASSISTANT_HINT, ASSISTANT_LANE, ASSISTANT_TITLE } from "@/lib/chatDock/labels";
import { useRouteReference } from "@/lib/chatDock/useRouteReference";
import { ASK_USER_RESPONSE_KEY } from "@/lib/askUserOptions";
import type { FirstClassBot } from "@/lib/whats-next/firstClassBots";
import { useAssistantComposer } from "@/lib/whats-next/useAssistantComposer";
import type { UseWhatsNextSession } from "@/lib/whats-next/useWhatsNextSession";

export default function ChatDock() {
  const dockCtx = useAssistantDock();
  const sessionCtx = useAssistantSession();
  if (!dockCtx || !sessionCtx?.bot) return null;
  return (
    // The transcript and composer read the run store; re-enter the
    // assistant's isolated one (the app around us sees the default).
    <AssistantStoreScope>
      <AssistantDock
        bot={sessionCtx.bot}
        session={sessionCtx.session}
        dock={dockCtx.dock}
        onDockChange={dockCtx.setDock}
      />
    </AssistantStoreScope>
  );
}

function AssistantDock({
  bot,
  session,
  dock,
  onDockChange,
}: {
  bot: FirstClassBot;
  session: UseWhatsNextSession;
  dock: DockState;
  onDockChange: (next: DockState) => void;
}) {
  const { reference, active, dismissed, dismiss, restore } = useRouteReference();

  // The pointer rides on every message, not just the first: the operator
  // navigates mid-conversation, and the assistant's next turn must know
  // where they are now.
  const decorate = useCallback(
    (text: string) => withPageContext(text, active),
    [active],
  );
  const composer = useAssistantComposer({ bot, session, decorate });

  const unread = useUnreadWhileClosed(dock, session.messages.length);
  const needsAttention =
    !!composer.pendingHumanQuestion ||
    session.runStatus === "paused_waiting_human" ||
    session.runStatus === "paused_operator";

  return (
    <ChatDockShell
      dock={dock}
      onDockChange={onDockChange}
      title={ASSISTANT_TITLE}
      titleHint={ASSISTANT_HINT}
      lane={ASSISTANT_LANE}
      bubbleLabel="Open assistant"
      bubbleTitle="Ask the assistant about this page"
      attentionTitle="The assistant is waiting on you"
      unread={unread}
      attention={needsAttention}
      dockedRightMode="self"
    >
      <ContextChip
        reference={reference}
        dismissed={dismissed}
        onDismiss={dismiss}
        onRestore={restore}
      />
      {/* flex column: ChatTranscript's root is `flex-1 overflow-y-auto`
          and only scrolls as a flex child. */}
      <div className="flex-1 min-h-0 overflow-hidden flex flex-col">
        {session.messages.length === 0 ? (
          <EmptyState session={session} />
        ) : (
          <ChatTranscript
            messages={session.messages}
            bot={bot}
            busyMessageId={session.busyMessageId}
            // The pending turn's input lives in the composer below, same
            // contract as the /whats-next route.
            composerHandlesId={composer.pendingHumanQuestion?.id}
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
      </div>
      {session.errorMessage && (
        <div className="shrink-0 border-t border-danger/40 bg-danger-soft px-3 py-1.5 text-caption text-danger-fg">
          {session.errorMessage}
        </div>
      )}
      <div className="shrink-0 border-t border-border-default bg-surface-0">
        {(composer.options.length > 0 || composer.quickReplies.length > 0) && (
          <div className="flex flex-wrap gap-1.5 px-3 pt-2">
            {/* Chip failures already surface via the session's
                errorMessage banner — swallow the rethrow that exists for
                the composer's draft preservation. */}
            {composer.options.map((o) => (
              <Button
                key={o.id}
                variant="secondary"
                size="sm"
                disabled={composer.busyPending}
                onClick={() => void composer.submitPending(o.id).catch(() => {})}
              >
                {o.label}
              </Button>
            ))}
            {composer.quickReplies.map((q) => (
              <Button
                key={q}
                variant="secondary"
                size="sm"
                disabled={composer.busyPending}
                onClick={() => void composer.submitPending(q).catch(() => {})}
              >
                {q}
              </Button>
            ))}
          </div>
        )}
        {/* ask_user with options may disallow free text — the chips above
            are then the only input. */}
        {(!composer.pendingIsAskUser ||
          composer.allowFreeText ||
          composer.options.length === 0) && (
          <div className="px-3 py-2">
            <AgentChatboxInline
              runId={session.runId ?? ""}
              compact={dock === "floating"}
              embedded
              placeholder={composerPlaceholder(
                session.runStatus,
                !!composer.pendingHumanQuestion,
              )}
              onSend={composer.onComposerSend}
            />
          </div>
        )}
      </div>
    </ChatDockShell>
  );
}

// No session yet: the dock's composer IS the launcher — the first
// message seeds a fresh run (useAssistantComposer's "closed" branch), so
// there is nothing to configure here, only to say so.
function EmptyState({ session }: { session: UseWhatsNextSession }) {
  return (
    <div className="flex-1 min-h-0 grid place-items-center px-4 text-center">
      <p className="text-caption text-fg-muted">
        {session.status === "launching"
          ? "Starting a session…"
          : "Ask about the page you're on — the first message starts a session."}
      </p>
    </div>
  );
}

// Unread badge for the closed bubble. The baseline is the message count
// at the moment the dock closed, so messages that arrive while the
// operator is elsewhere are counted, and opening clears it. Kept here
// rather than in the shell because only this caller knows what a
// "message" is.
function useUnreadWhileClosed(dock: DockState, messageCount: number): number {
  const closed = dock === "closed";
  // While open, everything on screen is read, so the baseline tracks the
  // count. It therefore holds the count as of the moment the dock closed
  // — which is exactly what the badge must measure against.
  const baselineRef = useRef(messageCount);
  useEffect(() => {
    if (!closed) baselineRef.current = messageCount;
  }, [closed, messageCount]);
  return closed ? Math.max(0, messageCount - baselineRef.current) : 0;
}
