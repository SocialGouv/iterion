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

import { useCallback, useEffect } from "react";
import { useLocation } from "wouter";

import {
  AssistantStoreScope,
  useAssistantDock,
  useAssistantSession,
} from "@/components/ChatDock/AssistantProvider";
import ContextChip from "@/components/ChatDock/ContextChip";
import { ChatDockShell } from "@/components/ChatDock/ChatDockShell";
import AgentChatboxInline from "@/components/shared/AgentChatboxInline";
import { Button } from "@/components/ui/Button";
import { InlineBanner } from "@/components/ui/InlineBanner";
import ChatTranscript from "@/components/WhatsNext/ChatTranscript";
import ResumeFooter from "@/components/WhatsNext/whatsNextView/ResumeFooter";
import { composerPlaceholder } from "@/components/WhatsNext/whatsNextView/composerPlaceholder";
import { withPageContext } from "@/lib/chatDock/contextMessage";
import type { DockState } from "@/lib/chatDock/dockState";
import { ASSISTANT_HINT, ASSISTANT_LANE, ASSISTANT_TITLE } from "@/lib/chatDock/labels";
import { isAssistantOwnRoute } from "@/lib/chatDock/routeReference";
import { useRouteReference } from "@/lib/chatDock/useRouteReference";
import { useUnreadWhileClosed } from "@/lib/chatDock/useUnreadWhileClosed";
import { ASK_USER_RESPONSE_KEY } from "@/lib/askUserOptions";
import type { FirstClassBot } from "@/lib/whats-next/firstClassBots";
import { useAssistantComposer } from "@/lib/whats-next/useAssistantComposer";
import type { UseWhatsNextSession } from "@/lib/whats-next/useWhatsNextSession";

export default function ChatDock() {
  const dockCtx = useAssistantDock();
  const sessionCtx = useAssistantSession();
  const [location] = useLocation();
  if (!dockCtx || !sessionCtx?.bot) return null;
  // /whats-next renders this same session full-width — the dock would be
  // a second composer over one conversation.
  if (isAssistantOwnRoute(location)) return null;
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
      {session.runId &&
      (session.runStatus === "failed_resumable" ||
        session.runStatus === "cancelled") ? (
        // Terminal-but-resumable wins over the composer, same rule as the
        // route view: queueing into a dead run only produces an error,
        // and a `failed_resumable` run is NOT one the composer re-seeds.
        <div className="shrink-0">
          <ResumeFooter
            runStatus={session.runStatus}
            busy={session.status === "submitting"}
            onResume={() => void session.resume()}
          />
        </div>
      ) : (
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
                // The dock's composer is live before a session exists (the
                // first message launches one) — but NOT while one is
                // already on its way. Startup discovery runs on every page
                // load and takes seconds on a cold cloud load; a send in
                // that window seeded a second Nexie beside the one about
                // to attach.
                disabled={composer.launchPending}
                placeholder={
                  composer.launchPending
                    ? "Starting a session…"
                    : composerPlaceholder(
                        session.runStatus,
                        !!composer.pendingHumanQuestion,
                      )
                }
                onSend={composer.onComposerSend}
              />
            </div>
          )}
        </div>
      )}
    </ChatDockShell>
  );
}

// No session yet: the dock's composer IS the launcher — the first
// message seeds a fresh run (useAssistantComposer's "closed" branch), so
// there is nothing to configure here, only to say so.
//
// Except when discovery FAILED: "no session found" and "couldn't look"
// are different states, and here they are one keystroke from a blind
// double-launch. Say so with the same words as SessionLauncher, the
// sibling that renders this session full-width.
function EmptyState({ session }: { session: UseWhatsNextSession }) {
  if (session.discoveryError) {
    return (
      <div className="flex-1 min-h-0 overflow-y-auto p-3">
        <InlineBanner
          tone="warning"
          layout="inline"
          title="Couldn't check for a running session"
          action={
            <Button variant="secondary" size="sm" onClick={session.retryDiscovery}>
              Retry
            </Button>
          }
        >
          {session.discoveryError} — starting one now may run two sessions in
          parallel.
        </InlineBanner>
      </div>
    );
  }
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

// AssistantDockCrashed is the dock's error-boundary fallback: it renders
// nothing, and on the way in it puts the dock back to `closed`.
//
// The rendering half is the easy half — a corner surface that crashed
// should disappear, not paint an error card over the page. The state half
// is the one that bites: AppShell reserves the 380px column from the dock
// STATE held in AssistantDockContext, which a crash inside ChatDock does
// not touch. So a dock that died while `docked-right` left a permanent
// dead band down the right of every authenticated route, and the control
// that would have cleared it (the minimise button) was inside the thing
// that crashed.
export function AssistantDockCrashed() {
  const ctx = useAssistantDock();
  const setDock = ctx?.setDock;
  const docked = ctx?.dock === "docked-right";
  useEffect(() => {
    if (docked && setDock) setDock("closed");
  }, [docked, setDock]);
  return null;
}
