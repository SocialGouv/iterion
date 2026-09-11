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
// What it adds over the route view is a first-message anchor: the page where
// the operator starts the question stays visible and navigable for the whole
// conversation. Later pages enter only through an explicit one-message join.

import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type DragEvent as ReactDragEvent,
} from "react";
import { useLocation, useSearch } from "wouter";

import {
  AssistantStoreScope,
  useAssistantDock,
  useAssistantSession,
} from "@/components/ChatDock/AssistantProvider";
import ContextChip from "@/components/ChatDock/ContextChip";
import { ChatDockShell } from "@/components/ChatDock/ChatDockShell";
import AttachedReferences from "@/components/ChatDock/AttachedReferences";
import BotSwitcher from "@/components/ChatDock/BotSwitcher";
import {
  attachReference,
  detachReference,
  hasReferenceDrag,
  readReferenceDrop,
  referenceDropEffect,
} from "@/lib/chatDock/dragReference";
import type { TypedReference } from "@/lib/chatDock/routeReference";
import AgentChatboxInline from "@/components/shared/AgentChatboxInline";
import VeilleBanner from "@/components/ChatDock/VeilleBanner";
import { useRunVeille } from "@/lib/chatDock/useRunVeille";
import { Button } from "@/components/ui/Button";
import { Checkbox } from "@/components/ui/Checkbox";
import { InlineBanner } from "@/components/ui/InlineBanner";
import ChatTranscript from "@/components/WhatsNext/ChatTranscript";
import AssistantApprovalComposer from "@/components/WhatsNext/AssistantApprovalComposer";
import ResumeFooter from "@/components/WhatsNext/whatsNextView/ResumeFooter";
import { composerPlaceholder } from "@/components/WhatsNext/whatsNextView/composerPlaceholder";
import {
  withActiveEditorDocument,
  withPageContext,
  withResolvedAssistantContext,
  initialPageContextFromMessage,
} from "@/lib/chatDock/contextMessage";
import { captureActiveEditorDocument } from "@/lib/chatDock/editorSession";
import {
  ASSISTANT_FLOATING_SIZE_KEY,
  type DockState,
} from "@/lib/chatDock/dockState";
import { ASSISTANT_HINT, ASSISTANT_LANE, ASSISTANT_TITLE } from "@/lib/chatDock/labels";
import {
  dockStandsDown,
  hrefForReference,
  referenceFromWire,
} from "@/lib/chatDock/routeReference";
import { useRouteReference } from "@/lib/chatDock/useRouteReference";
import { resolveAssistantContext } from "@/api/assistantContext";
import type { FirstClassBot } from "@/lib/whats-next/firstClassBots";
import {
  assistantHumanAnswer,
  useAssistantComposer,
} from "@/lib/whats-next/useAssistantComposer";
import { useNewSessionAction } from "@/lib/whats-next/useNewSessionAction";
import { useQueryClient } from "@tanstack/react-query";

import {
  SIDEBAR_COLLAPSED_PX,
  SIDEBAR_EXPANDED_PX,
} from "@/components/shared/Sidebar";
import { useUIStore } from "@/store/ui";

import { DraftBotOffer } from "@/components/ChatDock/draftBotOffer";
import {
  editorTargetForPage,
  EDITOR_OPENED_CONFIRMATION,
  navigationTargetForReply,
  useNavigationReply,
} from "@/lib/chatDock/replyNavigation";
import EditorChangeOffer from "@/components/ChatDock/EditorChangeOffer";
import AssistantFileChangeOffer from "@/components/ChatDock/AssistantFileChangeOffer";
import AssistantActionOffer from "@/components/ChatDock/AssistantActionOffer";
import {
  ConversationStrip,
  WorkplaceLink,
} from "@/components/ChatDock/ConversationStrip";
import {
  conversationContextState,
  type ConversationAnchor,
} from "@/lib/chatDock/conversations";
import { editorDraftKey, useDraftState } from "@/hooks/useDraftBot";
import {
  botDeclaresReviewer,
  readAskBeforeStart,
  readReviewer,
  writeAskBeforeStart,
  writeReviewer,
} from "@/lib/chatDock/assistantPrefs";
import { shouldRenderAssistantActionOffer } from "@/lib/chatDock/assistantActionVisibility";


import type { UseWhatsNextSession } from "@/lib/whats-next/useWhatsNextSession";

export default function ChatDock() {
  const dockCtx = useAssistantDock();
  const sessionCtx = useAssistantSession();
  const [location] = useLocation();
  if (!dockCtx || !sessionCtx?.bot) return null;
  // /whats-next renders this same session full-width — the dock would be a
  // second composer over one conversation.
  if (dockStandsDown(location)) return null;
  return (
    // The transcript and composer read the run store; re-enter the
    // assistant's isolated one (the app around us sees the default).
    <AssistantStoreScope>
      <AssistantDock
        // Both axes reset the dock's own state (attached refs, unread count):
        // a conversation switch commonly keeps the same bot.
        key={`${dockCtx.activeConversationId ?? "none"}:${sessionCtx.bot.id}`}
        bot={sessionCtx.bot}
        bots={sessionCtx.bots}
        onSelectBot={sessionCtx.selectBot}
        session={sessionCtx.session}
        dock={dockCtx.dock}
        onDockChange={dockCtx.setDock}
        dockWidth={dockCtx.dockWidth}
        onWidthChange={dockCtx.setDockWidth}
      />
    </AssistantStoreScope>
  );
}

function AssistantDock({
  bot,
  bots,
  onSelectBot,
  session,
  dock,
  onDockChange,
  dockWidth,
  onWidthChange,
}: {
  bot: FirstClassBot;
  bots: readonly FirstClassBot[];
  onSelectBot: (id: string) => void;
  session: UseWhatsNextSession;
  dock: DockState;
  onDockChange: (next: DockState) => void;
  dockWidth: number;
  onWidthChange: (px: number) => void;
}) {
  const { reference, page: activePage } = useRouteReference();
  const dockCtx = useAssistantDock();
  const activeConversation =
    dockCtx?.conversations.find(
      (conversation) => conversation.id === dockCtx.activeConversationId,
    ) ?? null;
  const [route] = useLocation();
  const search = useSearch();
  const currentHref = studioHref(route, search);
  const contextState = activeConversation
    ? conversationContextState(activeConversation, session.messages.length > 0)
    : "pending";
  const anchorReference =
    contextState === "anchored" && activeConversation?.origin
      ? referenceFromWire(
          activeConversation.origin,
          activeConversation.originLabel,
        )
      : null;
  const contextDisplayState =
    contextState === "anchored" && !anchorReference ? "unknown" : contextState;
  const anchorHref = activeConversation?.origin
    ? activeConversation.originHref ?? hrefForReference(activeConversation.origin)
    : null;
  // One predicate for every "are we already there?" question, so the back
  // link and the join offer cannot disagree. The href test alone is
  // query-sensitive (`sameStudioHref` normalises AND compares the query), so
  // `/pipelines?filter=x` used to read as a different page from the
  // `/pipelines` anchor — offering to join the page you are standing on.
  // Same reference means same subject, which is what the anchor points at.
  const currentlyAtAnchor =
    contextState === "anchored" &&
    ((!!anchorHref && sameStudioHref(anchorHref, currentHref)) ||
      (!!anchorReference && anchorReference.ref === reference?.ref));
  const backHref =
    contextState === "anchored" && anchorHref && !currentlyAtAnchor
      ? anchorHref
      : null;

  // Legacy conversations predate first-message anchors. Their persisted seed
  // is authoritative; the route visible during migration never is.
  useEffect(() => {
    if (!dockCtx || !activeConversation) return;
    const state = activeConversation.contextState;
    // `pending` can survive a crash between accepting the first message and
    // committing its anchor. Treat that explicit state like the legacy
    // unmarked shape once the conversation has demonstrably started.
    // `unknown` replays too, but for RECOVERY only (below): a conversation
    // finalised before its transcript had loaded must not stay stranded.
    if (state && state !== "pending" && state !== "unknown") return;
    if (!activeConversation.runId && session.messages.length === 0) return;
    const recovered = firstMessagePageContext(session.messages);
    if (recovered) {
      dockCtx.anchorConversation(activeConversation.id, {
        ref: recovered.reference.ref,
        label: recovered.reference.label,
        href:
          recovered.href ??
          activeConversation.originHref ??
          hrefForReference(recovered.reference.ref) ??
          undefined,
      });
      return;
    }
    // Past here we are FINALISING a conversation whose opening pointer cannot
    // be read from its own transcript. `unknown` is already that verdict —
    // re-running it would only churn the store.
    if (state === "unknown") return;
    // A loaded transcript with no machine header may still have the old
    // creation-time origin. Keep it as the documented migration fallback.
    //
    // Require the transcript itself, not `runStatus`: both come from the same
    // snapshot, and the opening message is reconstructed from that snapshot's
    // launch var, so a non-null status with an empty transcript means a run
    // with nothing to read rather than a transcript still in flight. Failing
    // towards `pending` keeps such a conversation re-anchorable.
    if (session.messages.length > 0) {
      const fallback = activeConversation.origin
        ? referenceFromWire(
            activeConversation.origin,
            activeConversation.originLabel,
          )
        : null;
      if (fallback) {
        dockCtx.anchorConversation(activeConversation.id, {
          ref: fallback.ref,
          label: fallback.label,
          href:
            activeConversation.originHref ??
            hrefForReference(fallback.ref) ??
            undefined,
        });
      } else {
        dockCtx.markContextUnknown(activeConversation.id);
      }
    }
  }, [dockCtx, activeConversation, session.messages]);

  // The floating panel grows leftward from its top-left handle, so its width
  // budget must stop at the sidebar — not at the viewport edge. Focus mode
  // drops the chrome entirely, so there is nothing to clear.
  const focusMode = useUIStore((s) => s.expanded);
  const sidebarCollapsed = useUIStore((s) => s.sidebarCollapsed);
  const leftInset = focusMode
    ? 0
    : sidebarCollapsed
      ? SIDEBAR_COLLAPSED_PX
      : SIDEBAR_EXPANDED_PX;

  // What the operator DROPPED in, cleared once it has gone out with a
  // message. Held here rather than in the session because it belongs to the
  // message being composed, not to the conversation.
  const [attached, setAttached] = useState<readonly TypedReference[]>([]);
  const navigationAttachmentRef = useRef<TypedReference | null>(null);
  const [dragOver, setDragOver] = useState(false);
  const detach = useCallback((ref: string) => {
    setAttached((cur) => detachReference(cur, ref));
  }, [setAttached]);

  const initialReference = contextState === "pending" ? reference : null;
  const initialPage = initialReference ? activePage : null;
  const conversationStarted =
    contextState === "anchored" ||
    contextState === "unknown" ||
    !!activeConversation?.runId ||
    session.messages.length > 0;
  const currentPageAttached =
    !!reference && attached.some((candidate) => candidate.ref === reference.ref);
  // Without an anchor there is no origin page to distinguish this one from,
  // so the offer would be permanent noise rather than a choice.
  const canJoinCurrentPage =
    conversationStarted &&
    !!anchorReference &&
    !!reference &&
    !currentlyAtAnchor &&
    !currentPageAttached;

  // The implicit page snapshot establishes the conversation once. Explicit
  // attachments and the live editor bridge remain per-message facilities.
  // Start both asynchronous captures from this immutable render snapshot so
  // navigation during either request cannot move the message from A to B.
  const decorate = useCallback(
    async (text: string) => {
      const navigationAttachment = navigationAttachmentRef.current;
      const messageAttached = navigationAttachment
        ? attachReference(attached, navigationAttachment)
        : attached;
      let decorated = withPageContext(
        text,
        initialReference,
        messageAttached,
        initialPage,
      );
      const resolvable = [initialReference, ...messageAttached]
        .filter(
          (candidate): candidate is TypedReference =>
            candidate !== null &&
            (candidate.kind === "run" ||
              candidate.kind === "node" ||
              candidate.kind === "card"),
        )
        .map((candidate) => candidate.ref)
        .filter((candidate, index, all) => all.indexOf(candidate) === index);
      const resolvePromise = resolveAssistantContext(resolvable, text);
      const captureEditor =
        bot.editor?.context === true &&
        activePage?.route.startsWith("/editor") &&
        (!!initialReference ||
          currentlyAtAnchor ||
          (!!reference &&
            messageAttached.some(
              (candidate) => candidate.ref === reference.ref,
            )));
      const editorPromise = captureEditor
        ? captureActiveEditorDocument(messageAttached)
        : Promise.resolve(null);
      const [resolved, editor] = await Promise.allSettled([
        resolvePromise,
        editorPromise,
      ]);
      if (resolved.status === "fulfilled") {
        // Always ask the host, even on a generic route: an operator may type
        // a Pipeline task id (for example #native:...) directly in the prose.
        // The server deterministically follows that task's last_run_id; the
        // model must never be asked to guess whether the id names a card or a
        // run.
        decorated = withResolvedAssistantContext(decorated, resolved.value);
      } else {
        // A context lookup must never swallow the operator's message. The
        // typed pointer remains visible and the assistant must say that it
        // could not verify it instead of inventing a store path.
      }
      if (editor.status === "fulfilled") {
        return withActiveEditorDocument(decorated, editor.value);
      } else {
        // Context is an affordance, not a reason to lose the operator's
        // message. Without a current editor-session marker, Copi cannot emit
        // an applicable proposal for the unsaved buffer.
        return decorated;
      }
    },
    [
      initialReference,
      initialPage,
      attached,
      bot.editor?.context,
      activePage,
      currentlyAtAnchor,
      reference,
    ],
  );
  const composer = useAssistantComposer({ bot, session, decorate });
  // A standby is not a pause waiting on the operator, and the dock is the
  // only place that difference is visible. The composer stays live underneath
  // it: writing during a standby does not cancel it.
  const veille = useRunVeille(session.runId ?? null);
  const submitComposerMessage = composer.onComposerSend;
  const decoratesComposerMessage = composer.willDecorateMessage;

  // Consume attachments only after a message really leaves. Suggested
  // replies use this same path: a navigate-then-send reply explicitly joins
  // its destination for one message and may carry its active editor document.
  const onComposerSend = useCallback(
    async (text: string, opts: { skills: string[] }) => {
      // Freeze the anchor before any context lookup or editor capture awaits.
      // The operator may navigate while those requests are in flight.
      const anchorAtSend: ConversationAnchor | null = initialReference
        ? {
            ref: initialReference.ref,
            label: initialReference.label,
            href: currentHref,
          }
        : null;
      const conversationIdAtSend = activeConversation?.id ?? null;
      await submitComposerMessage(text, opts);
      if (decoratesComposerMessage) {
        if (anchorAtSend && conversationIdAtSend && dockCtx) {
          dockCtx.anchorConversation(conversationIdAtSend, anchorAtSend);
        }
        navigationAttachmentRef.current = null;
        setAttached([]);
      }
    },
    [
      submitComposerMessage,
      decoratesComposerMessage,
      initialReference,
      currentHref,
      activeConversation,
      dockCtx,
      setAttached,
    ],
  );
  const sendSuggestedReply = useCallback(
    (message: string, targetRef?: string) => {
      navigationAttachmentRef.current = targetRef
        ? referenceFromWire(targetRef)
        : null;
      return onComposerSend(message, { skills: [] }).finally(() => {
        navigationAttachmentRef.current = null;
      });
    },
    [onComposerSend],
  );
  const navigationReply = useNavigationReply(sendSuggestedReply);
  // The chat equivalent of a CLI's `/new`. Lives in the dock's own chrome
  // because a bot that answers ONLY here (Copi) would otherwise have no way
  // to end a conversation — /whats-next's header is Nexie's, not his.
  const newSession = useNewSessionAction({ bot, session });

  // Same query key as the draft offer, so this costs nothing extra.
  const workplaceDraft = useDraftState(session.runId, session.messages.length);

  // The strip and the immutable anchor both read the dock context, which is where
  // the conversations live (the session context changes on every event and
  // would re-render the strip with it).
  const strip = {
    conversations: dockCtx?.conversations ?? [],
    activeConversationId: dockCtx?.activeConversationId ?? null,
    atConversationLimit: dockCtx?.atConversationLimit ?? false,
    selectConversation: dockCtx?.selectConversation ?? (() => {}),
    closeConversationById:
      dockCtx?.closeConversationById ?? (async () => {}),
    closingConversationIds:
      dockCtx?.closingConversationIds ?? new Set<string>(),
    waitingConversationIds:
      dockCtx?.waitingConversationIds ?? new Set<string>(),
    unreadWatchConversationIds:
      dockCtx?.unreadWatchConversationIds ?? new Set<string>(),
    openConversation: dockCtx?.openConversation ?? (() => {}),
    labelFor: (c: { botId: string }) =>
      bots.find((b) => b.id === c.botId)?.label ?? bot.label,
  };

  // Tell an open editor tab when there is something new to read.
  //
  // The draft is a node OUTPUT — written at `artifact_written` when the
  // first agent pass finishes, which is BEFORE the optional review, Copi's
  // revision and the chat pause — so this is the earliest instant anything could have
  // changed, and the tab needs no clock of its own. The conversation drives
  // the canvas instead of the canvas guessing. A refresh of an already-open
  // tab is a read; the offers below, which can WRITE, wait for the pause.
  const queryClient = useQueryClient();
  const turnCount = session.messages.length;
  useEffect(() => {
    if (!session.runId) return;
    void queryClient.invalidateQueries({
      queryKey: editorDraftKey(session.runId),
    });
  }, [session.runId, turnCount, queryClient]);

  const onDrop = useCallback((e: ReactDragEvent) => {
    setDragOver(false);
    const dropped = readReferenceDrop(e.dataTransfer);
    // Not a reference drag (a file, a stray selection): let the browser have
    // it rather than swallowing the event.
    if (!dropped) return;
    e.preventDefault();
    e.stopPropagation();
    setAttached((cur) => attachReference(cur, dropped));
  }, [setAttached, setDragOver]);

  // An offer belongs to a reply. The offers are built from the latest run
  // artifact carrying their field, and the agent node publishes that
  // artifact BEFORE the optional review/revision runs and before the chat node
  // pauses — on run 01a04999 the artifact landed at 18:20:03, the reviewer
  // wrote until 18:20:26, and the action card was clickable (or, under an
  // `auto` policy, self-executing) the whole time, i.e. before Copi had
  // received and integrated the private critique. So the offers
  // render only once the turn is parked on its chat pause. Not on an
  // `ask_user` pause mid-turn: the artifact of that moment is the previous
  // turn's, and its offers would be stale. This assumes a chat bot parks its
  // turn on a `human` node (Copi's and Nexie's `chat`), never on `ask_user`.
  // Corollary, deliberate: a turn that never reaches its pause (the run
  // failed or was cancelled between the agent node and `chat`) offers
  // nothing — an action whose answer was never finalized must not execute.
  const turnParked =
    !!composer.pendingHumanQuestion && !composer.pendingIsAskUser;
  const actionOfferVisible = shouldRenderAssistantActionOffer({
    runStatus: session.runStatus,
    hasPendingHumanQuestion: !!composer.pendingHumanQuestion,
    pendingIsAskUser: composer.pendingIsAskUser,
  });
  const unreadWatchCount = strip.unreadWatchConversationIds.size;
  const unreadWatchLabel = `${unreadWatchCount} new assistant ${
    unreadWatchCount === 1 ? "update" : "updates"
  }`;

  return (
    <ChatDockShell
      dock={dock}
      onDockChange={onDockChange}
      title={ASSISTANT_TITLE}
      titleHint={ASSISTANT_HINT}
      headerSlot={
        <>
          <BotSwitcher
            bots={bots}
            current={bot}
            onSelect={onSelectBot}
            busy={
              !!activeConversation &&
              strip.closingConversationIds.has(activeConversation.id)
            }
          />
          <ConversationStrip
            conversations={strip.conversations}
            activeId={strip.activeConversationId}
            atLimit={strip.atConversationLimit}
            onSelect={strip.selectConversation}
            onClose={strip.closeConversationById}
            onOpen={strip.openConversation}
            labelFor={(c) => strip.labelFor(c)}
            closingIds={strip.closingConversationIds}
            waitingIds={strip.waitingConversationIds}
            unreadWatchIds={strip.unreadWatchConversationIds}
          />

        </>
      }
      lane={ASSISTANT_LANE}
      leftInset={leftInset}
      bubbleLabel="Open assistant"
      bubbleTitle="Ask the assistant about this page"
      attentionTitle={unreadWatchLabel}
      badgeCount={unreadWatchCount}
      badgeAccessibleLabel={
        unreadWatchCount > 0 ? unreadWatchLabel : undefined
      }
      attention={unreadWatchCount > 0}
      openOnReferenceDrag
      dockedRightMode="self"
      dockedWidth={dockWidth}
      onWidthChange={onWidthChange}
      floatingSizeKey={ASSISTANT_FLOATING_SIZE_KEY}
    >
      {/* The confirm for "new conversation". Rendered inside the dock so it
          is reachable from wherever the dock is — the assistant is not
          route-bound and neither is ending its session. */}
      {newSession.dialog}
      {/* The whole dock body is the drop target, not just the textarea: an
          operator dragging a run card aims at the panel, and a 40px-tall
          bullseye is a feature nobody finds. dragOver only lights up for a
          drag that actually carries a reference — `types` is readable during
          dragover even though `getData` is not. */}
      <div
        className={`flex-1 min-h-0 flex flex-col ${
          dragOver ? "outline outline-2 -outline-offset-2 outline-accent" : ""
        }`}
        onDragOver={(e) => {
          if (!hasReferenceDrag(e.dataTransfer)) return;
          // preventDefault is what makes an element a valid drop target;
          // without it the browser refuses the drop and shows a "no" cursor.
          e.preventDefault();
          e.dataTransfer.dropEffect = referenceDropEffect(e.dataTransfer);
          setDragOver(true);
        }}
        onDragLeave={(e) => {
          // Only when the pointer leaves the panel itself — dragging across a
          // child fires dragleave for the child and would flicker the outline.
          if (e.currentTarget.contains(e.relatedTarget as Node | null)) return;
          setDragOver(false);
        }}
        onDrop={onDrop}
      >
        <ContextChip
          state={contextDisplayState}
          reference={contextState === "anchored" ? anchorReference : reference}
          backHref={backHref}
          onDismiss={
            contextState === "pending" && activeConversation && dockCtx
              ? () =>
                  dockCtx.setConversationContextEnabled(
                    activeConversation.id,
                    false,
                  )
              : undefined
          }
          onRestore={
            contextState === "disabled" &&
            !conversationStarted &&
            activeConversation &&
            dockCtx
              ? () =>
                  dockCtx.setConversationContextEnabled(
                    activeConversation.id,
                    true,
                  )
              : undefined
          }
        />
        {/* flex column: ChatTranscript's root is `flex-1 overflow-y-auto`
            and only scrolls as a flex child. */}
        <div className="flex-1 min-h-0 overflow-hidden flex flex-col">
        {session.messages.length === 0 && session.runStatus !== "running" ? (
          <EmptyState session={session} bot={bot} />
        ) : (
          <ChatTranscript
            messages={session.messages}
            bot={bot}
            busyMessageId={session.busyMessageId}
            showRunningStatus={session.runStatus === "running"}
            // The pending turn's input lives in the composer below, same
            // contract as the /whats-next route.
            composerHandlesId={composer.pendingHumanQuestion?.id}
            // One footer branch always owns a pending gate here: the normal
            // composer/approval/chips branch or ResumeFooter for a terminal
            // but resumable run. Keep stale transcript forms hidden in both.
            footerOwnsPendingInput={!!composer.pendingHumanQuestion}
            bubbleSlot={
              turnParked || actionOfferVisible ? (
                <>
                  {turnParked && (
                    <>
                      <DraftBotOffer
                        runId={session.runId}
                        revision={session.messages.length}
                      />
                      {bot.editor?.proposals === true && (
                        <>
                          <EditorChangeOffer
                            runId={session.runId}
                            revision={session.messages.length}
                          />
                          <AssistantFileChangeOffer
                            runId={session.runId}
                            revision={session.messages.length}
                          />
                        </>
                      )}
                    </>
                  )}
                  {actionOfferVisible && (
                    <AssistantActionOffer
                      runId={session.runId}
                      revision={session.messages.length}
                      workspaceHandoffId={
                        dockCtx?.activeWorkspaceHandoffId ?? null
                      }
                    />
                  )}
                </>
              ) : null
            }
            onHumanSubmit={(messageId, outcome) => {
              const m = session.messages.find((x) => x.id === messageId);
              if (!m || m.kind !== "human-question") return;
              void session
                .submitHumanAnswer(
                  messageId,
                  assistantHumanAnswer(bot, m, outcome),
                )
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
          <WorkplaceLink
            runId={session.runId}
            hasDraft={!!workplaceDraft.source}
            currentPath={route}
            currentSearch={search}
          />
          {canJoinCurrentPage && reference ? (
            <div className="px-3 pt-2">
              <Button
                variant="secondary"
                size="sm"
                onClick={() =>
                  setAttached((current) => attachReference(current, reference))
                }
              >
                Join this page
              </Button>
            </div>
          ) : null}
          <AttachedReferences references={attached} onDetach={detach} />
          {(composer.options.length > 0 ||
            composer.quickReplies.length > 0 ||
            (!!session.runId &&
              workplaceDraft.designing &&
              !workplaceDraft.source &&
              !route.startsWith("/editor"))) && (
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
                  key={`${q.label}:${q.message}`}
                  variant="secondary"
                  size="sm"
                  disabled={composer.busyPending || navigationReply.busy}
                  onClick={() => {
                    // Old string-only replies came from turns that could also
                    // render the separate editor venue button. While such a
                    // turn drains, fuse the reply with that venue instead of
                    // preserving the broken context-less path.
                    const target = navigationTargetForReply(
                      q,
                      workplaceDraft.designing &&
                        !workplaceDraft.source &&
                        !route.startsWith("/editor"),
                      reference,
                    );
                    if (target) {
                      navigationReply.submit(q.message, target);
                      return;
                    }
                    void sendSuggestedReply(q.message).catch(() => {});
                  }}
                >
                  {q.label}
                </Button>
              ))}
              {!!session.runId &&
                workplaceDraft.designing &&
                !workplaceDraft.source &&
                !route.startsWith("/editor") &&
                !composer.quickReplies.some((q) => !!q.navigateTo || q.legacy) && (
                  <Button
                    variant="secondary"
                    size="sm"
                    disabled={composer.busyPending || navigationReply.busy}
                    onClick={() =>
                      navigationReply.submit(
                        EDITOR_OPENED_CONFIRMATION,
                        editorTargetForPage(reference),
                      )
                    }
                  >
                    Open the editor
                  </Button>
                )}
            </div>
          )}
          {navigationReply.error && (
            <div className="px-3 pt-2 text-caption text-danger-fg">
              {navigationReply.error}
            </div>
          )}
          {veille.active && (
            <VeilleBanner
              issueIds={veille.issueIds}
              runTargets={veille.runTargets}
              busy={veille.busy}
              error={veille.error}
              onStop={() => void veille.stop()}
              assistantLabel={bot.label}
              isRunning={session.runStatus === "running"}
            />
          )}
          {composer.pendingApproval ? (
            <AssistantApprovalComposer
              hasTextField={!!composer.pendingApproval.textField}
              busy={composer.busyPending}
              onSubmit={composer.submitApproval}
            />
          ) : (
            /* ask_user with options may disallow free text — the chips above
               are then the only input. */
            (!composer.pendingIsAskUser ||
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
                        bot.label,
                      )
                }
                onSend={onComposerSend}
              />
            </div>
            )
          )}
        </div>
      )}
      </div>
    </ChatDockShell>
  );
}

function studioHref(path: string, search: string): string {
  const query = search.replace(/^\?/, "");
  return query ? `${path}?${query}` : path;
}

function sameStudioHref(left: string, right: string): boolean {
  const normalize = (value: string) => {
    try {
      const url = new URL(value, "https://studio.invalid");
      if (url.origin !== "https://studio.invalid") return null;
      const entries = Array.from(url.searchParams.entries()).sort(
        ([ak, av], [bk, bv]) =>
          ak === bk ? av.localeCompare(bv) : ak.localeCompare(bk),
      );
      const query = new URLSearchParams(entries).toString();
      return query ? `${url.pathname}?${query}` : url.pathname;
    } catch {
      return null;
    }
  };
  const a = normalize(left);
  const b = normalize(right);
  return a !== null && a === b;
}

function firstMessagePageContext(
  messages: UseWhatsNextSession["messages"],
) {
  for (const message of messages) {
    if (message.kind === "user-message") {
      return initialPageContextFromMessage(message.text);
    }
    if (message.kind === "human-question" && message.userReply !== undefined) {
      return initialPageContextFromMessage(message.userReply);
    }
  }
  return null;
}

// No session yet: the dock's composer IS the launcher — the first
// message seeds a fresh run (useAssistantComposer's "closed" branch), so
// there is nothing to configure here, only to say so.
//
// Except when discovery FAILED: "no session found" and "couldn't look"
// are different states, and here they are one keystroke from a blind
// double-launch. Say so with the same words as SessionLauncher, the
// sibling that renders this session full-width.
// Offered BEFORE a conversation exists, because review + refinement is priced
// per turn for the whole session — a switch buried in settings would be found
// after the money was spent. Dismissible for good: "don't ask again" keeps the
// saved answer and stops the question, and Settings → Assistant can change both.
function StartOptions({ bot }: { bot: FirstClassBot }) {
  const [reviewer, setReviewer] = useState(readReviewer);
  const [ask, setAsk] = useState(readAskBeforeStart);
  // Only for a bot whose manifest says it understands cross-review. Offering a
  // toggle that does nothing is worse than not offering one.
  if (!ask || !botDeclaresReviewer(bot)) return null;
  return (
    <div className="mb-3 rounded-md border border-border-subtle bg-surface-2 p-2.5 space-y-2 text-left">
      <p className="text-caption text-fg-muted">Before you start</p>
      <Checkbox
        checked={reviewer}
        onChange={(e) => {
          setReviewer(e.target.checked);
          writeReviewer(e.target.checked);
        }}
        label={
          <span>
            Review and refine each answer
            <span className="block text-caption text-fg-subtle">
              A second model privately critiques Copi, then Copi rewrites the
              answer. Costs up to two extra calls per turn.
            </span>
          </span>
        }
      />
      <Checkbox
        checked={!ask}
        onChange={(e) => {
          const nextAsk = !e.target.checked;
          setAsk(nextAsk);
          writeAskBeforeStart(nextAsk);
        }}
        label={
          <span className="text-caption text-fg-subtle">
            Don't ask again — change it in Settings → Assistant
          </span>
        }
      />
    </div>
  );
}

function EmptyState({
  session,
  bot,
}: {
  session: UseWhatsNextSession;
  bot: FirstClassBot;
}) {
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
  if (session.status === "launching") {
    return (
      <div className="flex-1 min-h-0 grid place-items-center px-4 text-center">
        <p className="text-caption text-fg-muted">Starting a session…</p>
      </div>
    );
  }
  return (
    <div className="flex-1 min-h-0 overflow-y-auto px-4 py-3 flex flex-col justify-center">
      <StartOptions bot={bot} />
      <p className="text-caption text-fg-muted text-center">
        Ask about the page you're on — the first message starts a session.
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
