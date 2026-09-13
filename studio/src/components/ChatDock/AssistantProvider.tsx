// One assistant session for the whole studio, mounted above the route
// tree so navigating cannot restart it or drop the transcript.
//
// Two things had to move out of WhatsNextView for that:
//
// 1. THE SESSION. It was a hook inside the route component, so leaving
//    /whats-next unmounted it and coming back re-ran discovery. It now
//    lives here — mounted once, for the lifetime of the authenticated
//    app. The session itself is already built for this: the whats-next
//    run is long-lived and parks on its `chat` node, budget-free, for
//    days.
//
// 2. THE RUN STORE. The session used the MODULE-DEFAULT run store, which
//    was tolerable while it only existed on one route. Mounted globally
//    it would permanently hold the assistant's run in the store every
//    shell-level consumer reads (useDocumentTitle would title /runs/:id
//    after the assistant's run). So the assistant gets a store of its
//    own, and this provider hands the DEFAULT store back to the subtree
//    below it. Surfaces that render the assistant's transcript re-enter
//    the assistant store through <AssistantStoreScope>.
//
// TWO contexts, deliberately. The session object changes on every
// websocket event; the dock state changes when the operator clicks. A
// single context would re-render every consumer — including AppShell,
// which reads the dock state to reserve the docked column — on each
// event, dragging the whole route subtree with it.
//
// Dock state is persisted per USER rather than per route: docking the
// assistant on /board must leave it docked on /runs.

import {
  createContext,
  memo,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  useSyncExternalStore,
  type ReactNode,
  useRef,
} from "react";
import { useLocation } from "wouter";

import {
  FLOATING_FOOTPRINT_PX,
} from "@/components/ChatDock/ChatDockShell";
import { ErrorBoundary } from "@/components/shared/ErrorBoundary";
import {
  dockStandsDown,
  isAssistantOwnRoute,
} from "@/lib/chatDock/routeReference";
import { errorMessage } from "@/lib/errorHints";
import { useConfirm } from "@/hooks/useConfirm";
import { cancelRun, listRuns, type RunSummary } from "@/api/runs";
import {
  bindWorkspaceHandoff,
  redeemWorkspaceHandoff,
} from "@/api/workspaceHandoff";
import {
  MAX_CONVERSATIONS,
  ACTIVE_CONVERSATION_KEY,
  CONVERSATIONS_KEY,
  addConversation,
  anchorConversation,
  claimRun,
  collectUnreadWatchConversationIds,
  collectWaitingConversationIds,
  conversationStorageKey,
  closeConversation,
  latestWatchResultSeq,
  markConversationContextUnknown,
  markConversationWatchResultRead,
  setConversationContextEnabled,
  switchConversationBot,
  newConversationId,
  readActiveConversation,
  readConversations,
  resolveActive,
  writeActiveConversation,
  writeConversations,
  type ConversationAnchor,
  type Conversation,
} from "@/lib/chatDock/conversations";
import {
  cancelThenDispose,
  runIdForDisposal,
  shouldConfirmRunDisposal,
} from "@/lib/chatDock/conversationDisposal";
import {
  ASSISTANT_BOT_KEY,
  ASSISTANT_DOCK_KEY,
  DOCK_BREAKPOINT_PX,
  DOCKED_WIDTH_DEFAULT_PX,
  clampDockWidth,
  readDockState,
  readDockWidth,
  openedDock,
  writeDockState,
  writeDockWidth,
  type DockState,
} from "@/lib/chatDock/dockState";
import { readStringFlag, writeStringFlag } from "@/lib/localStorageFlag";
import {
  STUDIO_CHAT_REAPABLE_STATUSES,
  planStudioChatReconciliation,
  readOrCreateStudioChatClientId,
} from "@/lib/chatDock/studioChatOwnership";
import { AssistantPageContextProvider } from "@/lib/chatDock/pageContext";
import { useChatRegistry } from "@/hooks/useChatRegistry";
import { DEFAULT_WHATS_NEXT_BOT_ID } from "@/lib/whats-next/firstClassBots";
import {
  type FirstClassBot,
} from "@/lib/whats-next/firstClassBots";
import {
  useWhatsNextSession,
  type UseWhatsNextSession,
  type WhatsNextSessionOptions,
} from "@/lib/whats-next/useWhatsNextSession";
import {
  createRunStore,
  getDefaultRunStore,
  RunStoreProvider,
  type RunStore,
} from "@/store/run";
import { useUIStore } from "@/store/ui";
import {
  postWorkspaceUnreadCount,
  useWorkspacePaneVisibility,
} from "@/lib/workspacePaneVisibility";

interface AssistantDockContextValue {
  store: RunStore;
  dock: DockState;
  setDock: (next: DockState) => void;
  // Whether the dock has a bot to render at all. Duplicated out of the
  // session context on purpose: useAssistantReservedWidthPx must not
  // read that context (it changes on every websocket event, and would
  // re-render the whole AppShell with it), but it must agree with
  // ChatDock about whether anything occupies the right edge — otherwise
  // the shell reserves 380px for a dock that rendered nothing. Cheap to
  // keep here because the lookup is a stable registry hit.
  hasSession: boolean;
  // Width of the docked-right column, in px. Shared through the DOCK context
  // (not the session one) because AppShell's layout reservation reads it and
  // must not re-render on every websocket event.
  dockWidth: number;
  setDockWidth: (px: number) => void;
  // The open conversations, and the one on screen. Several at a time, each
  // its own run — see lib/chatDock/conversations.
  conversations: Conversation[];
  activeConversationId: string | null;
  activeWorkspaceHandoffId: string | null;
  anchorConversation: (id: string, anchor: ConversationAnchor) => void;
  markContextUnknown: (id: string) => void;
  setConversationContextEnabled: (id: string, enabled: boolean) => void;
  openConversation: () => void;
  selectConversation: (id: string) => void;
  closeConversationById: (id: string) => Promise<void>;
  closingConversationIds: ReadonlySet<string>;
  // Conversation tabs whose attached run is parked on a human node. This is
  // deliberately a SET, not a message count: Copi's normal resting state is
  // paused_waiting_human, whether its last answer has been read or not.
  waitingConversationIds: ReadonlySet<string>;
  // Finalized automatic watch diagnoses that have not yet been visible in
  // their active, open conversation. A normal chat pause is not unread news.
  unreadWatchConversationIds: ReadonlySet<string>;
  atConversationLimit: boolean;
}

interface AssistantSessionContextValue {
  // Null only if the registry is empty. It has a built-in floor, so this is
  // the "no bots at all" case rather than a lookup miss — but the registry is
  // manifest-driven and therefore dynamic now, so every surface below
  // degrades rather than crashes.
  bot: FirstClassBot | null;
  session: UseWhatsNextSession;
  // Every conversational bot the server offers, and the switch between them.
  // Both live on the SESSION context, not the dock one: changing bots drops
  // the session, so a consumer that re-renders on this is already
  // re-rendering on the session.
  bots: FirstClassBot[];
  selectBot: (id: string) => void;
}

// What the keyed session engine hands back to its parent: the session value
// plus the key of the engine that produced it. The key never reaches
// consumers — it exists so the parent can refuse a publication that belongs
// to the conversation it just left.
interface PublishedSession {
  key: string;
  value: AssistantSessionContextValue;
}

const AssistantDockContext = createContext<AssistantDockContextValue | null>(null);
const AssistantSessionContext =
  createContext<AssistantSessionContextValue | null>(null);

// Keeps hook order valid on a registry miss (hooks must run
// unconditionally, so the session hook always gets a bot).
const FALLBACK_BOT: FirstClassBot = {
  id: "",
  label: "",
  description: "",
  workflowPath: "",
  launcherVars: [],
  nodeMap: {},
};

// Stands in whenever no session for the CURRENT key has been published: on
// the first paint, and again for the commits between a conversation switch
// and the new engine's first publication. An empty session is the honest
// answer there — the alternative is serving the previous conversation's.
const FALLBACK_SESSION = {
  status: "idle",
  runId: null,
  messages: [],
  busyMessageId: null,
  runStatus: null,
  errorMessage: null,
  lastVars: null,
  discoveryError: null,
  retryDiscovery: () => {},
  sessionRepo: null,
  launchRepo: null,
  launch: async () => {},
  submitHumanAnswer: async () => {},
  newSession: () => {},
  resume: async () => {},
} as unknown as UseWhatsNextSession;

export function AssistantProvider({ children }: { children: ReactNode }) {
  // One store for the whole app lifetime. Not the registry
  // (getOrCreateRunStore) — that is keyed by runId, and the assistant's
  // runId is only known after discovery.
  const store = useMemo(() => createRunStore(), []);
  const [location] = useLocation();
  return (
    <AssistantPageContextProvider>
      <RunStoreProvider store={store}>
        {/* The assistant must not be able to take the app down with it.
            Its host sits ABOVE the route tree — that is the whole point of
            the design — so it is also above every per-route
            <ErrorBoundary>, and a throw in the session hook or the
            transcript fold would unmount every route at once. Before the
            lift, the same fold ran inside "What's Next view"'s boundary
            and degraded exactly one page.

            The fallback is therefore the app WITHOUT an assistant, not an
            error card: every consumer already handles a null context
            (useAssistantDock returns null, the reserved width is 0, the
            dock renders nothing), so the operator keeps /board, /runs and
            the run console and merely loses the dock. */}
        <ErrorBoundary
          area="Assistant session"
          // A bad transcript/session fold degrades to the app without the
          // assistant, but navigation gets one fresh mount instead of making
          // that degradation permanent for the whole browser session.
          resetKey={location}
          fallback={
            <RunStoreProvider store={getDefaultRunStore()}>
              {children}
            </RunStoreProvider>
          }
        >
          <AssistantSessionHost>{children}</AssistantSessionHost>
        </ErrorBoundary>
      </RunStoreProvider>
    </AssistantPageContextProvider>
  );
}

// The conversation strip lives here; each conversation gets its own host
// below, and only the active one publishes context to the app.
//
// Why one host per conversation rather than one hook switching between them:
// a session is not a value you can swap, it is a live thing — a websocket, a
// discovery, a transcript fold. Mounting them separately is what lets a
// conversation KEEP RUNNING while you read another, which is the entire point
// of having more than one.
function AssistantSessionHost({ children }: { children: ReactNode }) {
  const registry = useChatRegistry();
  const [location] = useLocation();
  const addToast = useUIStore((state) => state.addToast);
  const { confirm: confirmDisposal, dialog: disposalDialog } = useConfirm();

  const [conversations, setConversations] = useState<Conversation[]>(() =>
    readConversations(),
  );
  const [activeId, setActiveId] = useState<string>(() =>
    readActiveConversation(),
  );
  // Local writes can land in the same tick (lazy-tab persistence, run claim,
  // first-message anchor). React state is not synchronously readable there,
  // so these refs are the serialized source for updater-style mutations.
  const conversationsRef = useRef(conversations);
  const activeIdRef = useRef(activeId);
  const [studioChatClientId] = useState(readOrCreateStudioChatClientId);
  const paneVisible = useWorkspacePaneVisibility();
  const [handoffPrompt, setHandoffPrompt] = useState<{
    conversationId: string;
    message: string;
  } | null>(null);
  const handoffRedeemStartedRef = useRef(false);

  // localStorage is shared by browser windows, React state is not. Mirror
  // tab-strip changes from sibling windows before startup reconciliation can
  // decide that one of their conversations is orphaned.
  useEffect(() => {
    const onStorage = (event: StorageEvent) => {
      if (event.key === conversationStorageKey(CONVERSATIONS_KEY)) {
        const next = readConversations();
        conversationsRef.current = next;
        setConversations(next);
      } else if (event.key === conversationStorageKey(ACTIVE_CONVERSATION_KEY)) {
        const next = readActiveConversation();
        activeIdRef.current = next;
        setActiveId(next);
      }
    };
    window.addEventListener("storage", onStorage);
    return () => window.removeEventListener("storage", onStorage);
  }, []);

  // One run store PER conversation, kept for the host's lifetime.
  //
  // Not one shared store: the session hook resets on a bot change, not on a
  // conversation change, so two conversations with the same bot would show
  // each other's run. Keeping them apart is also what lets a background
  // conversation still be there — transcript and all — when you come back.
  const storesRef = useRef<Map<string, RunStore>>(new Map());
  // Synchronous guard + reactive mirror. The ref closes the double-click race
  // before React commits the disabled button; state tells the strip what to
  // render. Deliberately not persisted: after a refresh the idempotent cancel
  // can be attempted again.
  const disposingRef = useRef<Set<string>>(new Set());
  const [closingConversationIds, setClosingConversationIds] = useState<Set<string>>(
    () => new Set(),
  );
  const beginDisposal = useCallback((id: string): boolean => {
    // useConfirm owns one resolver. Serialize disposal across conversations
    // so a second tab cannot replace the first confirmation and leave its
    // promise (and closing state) stranded forever.
    if (disposingRef.current.size > 0) return false;
    disposingRef.current.add(id);
    setClosingConversationIds(new Set(disposingRef.current));
    return true;
  }, []);
  const endDisposal = useCallback((id: string) => {
    disposingRef.current.delete(id);
    setClosingConversationIds(new Set(disposingRef.current));
  }, []);
  const storeFor = useCallback((id: string): RunStore => {
    const existing = storesRef.current.get(id);
    if (existing) return existing;
    const made = createRunStore();
    storesRef.current.set(id, made);
    return made;
  }, []);

  const persist = useCallback(
    (
      update:
        | Conversation[]
        | ((current: readonly Conversation[]) => Conversation[]),
      active?: string | null,
    ) => {
      const current = conversationsRef.current;
      const list = typeof update === "function" ? update(current) : update;
      const nextActive = active === undefined ? activeIdRef.current : active ?? "";
      conversationsRef.current = list;
      activeIdRef.current = nextActive;
      setConversations(list);
      writeConversations(list);
      setActiveId(nextActive);
      writeActiveConversation(nextActive);
    },
    [],
  );

  const reconciliationStartedRef = useRef(false);
  useEffect(() => {
    if (registry.loading || reconciliationStartedRef.current) return;
    reconciliationStartedRef.current = true;
    let disposed = false;
    let retryTimer: number | null = null;

    const reconcile = async () => {
      try {
        const batches = await Promise.all([
          // Recent terminal runs let a tab recover when the browser died
          // after createRun but before persisting runId and the run then ended.
          listRuns({ limit: 500 }),
          ...STUDIO_CHAT_REAPABLE_STATUSES.map((status) =>
            listRuns({ status, limit: 500 }),
          ),
        ]);
        if (disposed) return;
        const byId = new Map<string, RunSummary>();
        for (const run of batches.flat()) byId.set(run.id, run);

        // Read storage NOW, not the render closure: a sibling window may have
        // created/closed a conversation while the network requests were in
        // flight.
        let current = readConversations();
        const plan = planStudioChatReconciliation(
          Array.from(byId.values()),
          current,
          studioChatClientId,
          Date.now(),
        );

        if (plan.repairs.length > 0) {
          for (const repair of plan.repairs) {
            const claimed = claimRun(current, repair.conversationId, repair.runId);
            if (claimed) current = claimed;
          }
          const storedActive = readActiveConversation();
          persist(current, resolveActive(current, storedActive)?.id ?? null);
        }

        const cancelled = await Promise.allSettled(
          plan.cancelRunIds.map((runId) => cancelRun(runId)),
        );
        const failures = cancelled.filter(
          (result): result is PromiseRejectedResult => result.status === "rejected",
        );
        const firstFailure = failures[0];
        if (firstFailure) throw firstFailure.reason;

        // A young candidate gets exactly one maturity-time recheck. Skipping
        // it forever would turn the 60s race guard into a permanent leak.
        if (!disposed && plan.retryAfterMs !== null) {
          retryTimer = window.setTimeout(
            () => void reconcile(),
            Math.max(1, plan.retryAfterMs + 25),
          );
        }
      } catch (error) {
        if (disposed) return;
        addToast(`Could not reconcile assistant conversations: ${errorMessage(error)}`, "error", {
          persistent: true,
          action: { label: "Retry", onClick: () => void reconcile() },
        });
      }
    };

    void reconcile();
    return () => {
      disposed = true;
      if (retryTimer !== null) window.clearTimeout(retryTimer);
    };
  }, [registry.loading, studioChatClientId, persist, addToast]);

  // The dock always has at least one conversation to show. Created lazily, so
  // a browser that never opens the dock never persists one.
  const ensured = useMemo(() => {
    if (conversations.length > 0) return conversations;
    const seed: Conversation = {
      id: newConversationId(),
      botId: readStringFlag(ASSISTANT_BOT_KEY, ""),
      contextState: "pending",
    };
    return [seed];
  }, [conversations]);

  const active = resolveActive(ensured, activeId);

  const persistActiveBeforeLaunch = useCallback(() => {
    if (!active) return;
    // The lazy first tab exists only in `ensured`. Persist it BEFORE
    // createRun so another window cannot see a source-stamped run without
    // its owning conversation during the runId write gap.
    persist(
      (current) =>
        current.some((conversation) => conversation.id === active.id)
          ? [...current]
          : addConversation(current, active),
      activeIdRef.current || active.id,
    );
  }, [active, persist]);

  const openConversation = useCallback(() => {
    const opened: Conversation = {
      id: newConversationId(),
      botId: active?.botId ?? "",
      fresh: true,
      contextState: "pending",
    };
    const current = conversationsRef.current;
    const base =
      current.length > 0 || !active ? [...current] : addConversation(current, active);
    const next = addConversation(base, opened);
    if (next.length === base.length) return;
    persist(next, opened.id);
  }, [active, persist]);

  const selectConversation = useCallback(
    (id: string) => persist((current) => [...current], id),
    [persist],
  );

  const anchorConversationById = useCallback(
    (id: string, anchor: ConversationAnchor) => {
      const fallback = ensured.find((conversation) => conversation.id === id);
      persist((current) => {
        const base =
          current.some((conversation) => conversation.id === id) || !fallback
            ? [...current]
            : addConversation(current, fallback);
        return anchorConversation(base, id, anchor);
      });
    },
    [ensured, persist],
  );

  const markContextUnknown = useCallback(
    (id: string) => {
      const fallback = ensured.find((conversation) => conversation.id === id);
      persist((current) => {
        const base =
          current.some((conversation) => conversation.id === id) || !fallback
            ? [...current]
            : addConversation(current, fallback);
        return markConversationContextUnknown(base, id);
      });
    },
    [ensured, persist],
  );

  const setContextEnabled = useCallback(
    (id: string, enabled: boolean) => {
      const fallback = ensured.find((conversation) => conversation.id === id);
      persist((current) => {
        const base =
          current.some((conversation) => conversation.id === id) || !fallback
            ? [...current]
            : addConversation(current, fallback);
        return setConversationContextEnabled(base, id, enabled);
      });
    },
    [ensured, persist],
  );

  // Record the run a conversation launched, so every later mount attaches to
  // THAT one. Without it a remount falls back to the bot-scoped lookup, which
  // returns the latest run for the bot — another conversation's.
  const markActiveLaunched = useCallback(
    (runId: string) => {
      if (!active) return;
      if (!active.fresh && active.runId === runId) return;
      // claimRun refuses a run another conversation already owns. The session
      // hook can still be handed a neighbour's run — a conversation with no id
      // of its own falls back to the bot-scoped lookup — and recording that is
      // what turned a transient mix-up into a persisted one.
      persist((current) => {
        const base = current.some((conversation) => conversation.id === active.id)
          ? [...current]
          : addConversation(current, active);
        return claimRun(base, active.id, runId) ?? base;
      });
    },
    [active, persist],
  );

  const closeRetryRef = useRef<(id: string) => void>(() => {});
  const closeConversationById = useCallback(
    async (id: string) => {
      const conversation = conversationsRef.current.find(
        (candidate) => candidate.id === id,
      );
      if (!conversation || !beginDisposal(id)) return;
      const snapshot = storesRef.current.get(id)?.getState().snapshot;
      const runId = runIdForDisposal(conversation, snapshot);
      try {
        if (
          shouldConfirmRunDisposal(runId, snapshot?.run.status) &&
          !(await confirmDisposal({
            title: "Close conversation and stop its run?",
            message:
              "Closing this conversation stops its Iterion run and any run watches it owns. Its transcript remains available in the run console, but the assistant cannot continue this thread.",
            confirmLabel: "Close and stop run",
            confirmVariant: "danger",
          }))
        ) {
          return;
        }
        await cancelThenDispose({
          runId,
          dispose: () => {
            storesRef.current.delete(id);
            const current = conversationsRef.current;
            const got = closeConversation(current, id, activeIdRef.current);
            persist(got.list, got.activeId);
          },
        });
      } catch (error) {
        addToast(`Could not close the assistant conversation: ${errorMessage(error)}`, "error", {
          persistent: true,
          action: {
            label: "Retry close",
            onClick: () => closeRetryRef.current(id),
          },
        });
      } finally {
        endDisposal(id);
      }
    },
    [
      beginDisposal,
      confirmDisposal,
      persist,
      addToast,
      endDisposal,
    ],
  );
  useEffect(() => {
    closeRetryRef.current = (id) => void closeConversationById(id);
  }, [closeConversationById]);

  // Two lanes, still. Nexie owns /whats-next and answers there whatever the
  // dock's strip holds, so that route runs its OWN conversation rather than
  // taking over one of the operator's.
  const onNexieRoute = isAssistantOwnRoute(location);
  const nexieBot = registry.byId[DEFAULT_WHATS_NEXT_BOT_ID] ?? null;

  const switchRetryRef = useRef<(id: string) => void>(() => {});
  const selectBot = useCallback(
    async (id: string) => {
      if (!active) {
        writeStringFlag(ASSISTANT_BOT_KEY, id);
        return;
      }
      if (id === active.botId || !beginDisposal(active.id)) return;
      const snapshot = storesRef.current.get(active.id)?.getState().snapshot;
      const runId = runIdForDisposal(active, snapshot);
      try {
        if (
          shouldConfirmRunDisposal(runId, snapshot?.run.status) &&
          !(await confirmDisposal({
            title: "Switch assistant and stop the current run?",
            message:
              "Changing assistant ends the current conversation and stops its Iterion run. The transcript remains in the run console.",
            confirmLabel: "Stop run and switch",
            confirmVariant: "danger",
          }))
        ) {
          return;
        }
        await cancelThenDispose({
          runId,
          dispose: () => {
            storesRef.current.delete(active.id);
            writeStringFlag(ASSISTANT_BOT_KEY, id);
            persist(
              (current) => switchConversationBot(current, active.id, id),
            );
          },
        });
      } catch (error) {
        addToast(`Could not switch assistant: ${errorMessage(error)}`, "error", {
          persistent: true,
          action: {
            label: "Retry switch",
            onClick: () => switchRetryRef.current(id),
          },
        });
      } finally {
        endDisposal(active.id);
      }
    },
    [
      active,
      beginDisposal,
      confirmDisposal,
      persist,
      addToast,
      endDisposal,
    ],
  );
  useEffect(() => {
    switchRetryRef.current = (id) => void selectBot(id);
  }, [selectBot]);

  const [dock, setDockState] = useState<DockState>(() =>
    readDockState(ASSISTANT_DOCK_KEY, "closed"),
  );
  const setDock = useCallback((next: DockState) => {
    setDockState(next);
    writeDockState(ASSISTANT_DOCK_KEY, next);
  }, []);
  const consumeHandoffPrompt = useCallback(() => setHandoffPrompt(null), []);

  useEffect(() => {
    const ticket = new URLSearchParams(window.location.search).get("handoff");
    if (!ticket || handoffRedeemStartedRef.current) return;
    if (conversationsRef.current.length >= MAX_CONVERSATIONS) {
      addToast(
        "Close an assistant conversation before continuing this project handoff.",
        "error",
        { persistent: true },
      );
      return;
    }
    handoffRedeemStartedRef.current = true;
    const openedId = newConversationId();
    void redeemWorkspaceHandoff(ticket, studioChatClientId, openedId)
      .then((handoff) => {
        const url = new URL(window.location.href);
        url.searchParams.delete("handoff");
        window.history.replaceState(null, "", `${url.pathname}${url.search}${url.hash}`);
        const opened: Conversation = {
          id: openedId,
          botId: handoff.bot_id || "copilot",
          fresh: true,
          contextState: "pending",
          workspaceHandoffId: handoff.handoff_id,
        };
        persist((current) => addConversation(current, opened), opened.id);
        setHandoffPrompt({
          conversationId: opened.id,
          message: `Continue this cross-project task from project ${handoff.source_project_id}. Use this handoff summary as the starting context:\n\n${handoff.summary}`,
        });
        setDock(openedDock());
      })
      .catch((error) => {
        handoffRedeemStartedRef.current = false;
        addToast(`Could not continue the project handoff: ${errorMessage(error)}`, "error", {
          persistent: true,
          action: { label: "Retry", onClick: () => window.location.reload() },
        });
      });
  }, [addToast, persist, setDock, studioChatClientId]);

  const [dockWidth, setDockWidthState] = useState<number>(() =>
    readDockWidth(DOCKED_WIDTH_DEFAULT_PX),
  );
  const setDockWidth = useCallback((px: number) => {
    const next = clampDockWidth(px);
    setDockWidthState(next);
    writeDockWidth(next);
  }, []);

  const activeBot = onNexieRoute
    ? nexieBot
    : registry.resolveDock(active?.botId ?? "");
  const activeRunSource = useMemo(
    () =>
      !onNexieRoute && active
        ? {
            kind: "studio_chat" as const,
            client_id: studioChatClientId,
            conversation_id: active.id,
          }
        : undefined,
    [onNexieRoute, active, studioChatClientId],
  );

  // /whats-next runs Nexie's own conversation, so it gets its own store too
  // rather than borrowing whichever dock tab happens to be active.
  const activeStore = onNexieRoute
    ? storeFor("__whats-next")
    : storeFor(active?.id ?? "__none");

  // One subscription fans in every conversation-owned Zustand store. The
  // background pumps already keep those stores current; subscribing here is
  // enough to make their gate state visible without publishing their entire
  // rapidly-changing session facade through React context.
  const subscribeConversationStores = useCallback(
    (onStoreChange: () => void) => {
      const unsubscribers = ensured.map((conversation) =>
        storeFor(conversation.id).subscribe(onStoreChange),
      );
      return () => {
        for (const unsubscribe of unsubscribers) unsubscribe();
      };
    },
    [ensured, storeFor],
  );
  const conversationRuntimeSnapshot = useCallback(
    () =>
      JSON.stringify(
        ensured.map((conversation) => {
          const state = storeFor(conversation.id).getState();
          return [
            conversation.id,
            state.runId,
            state.snapshot?.run.status ?? null,
            latestWatchResultSeq(state.events),
          ];
        }),
      ),
    [ensured, storeFor],
  );
  const conversationRuntimeSignature = useSyncExternalStore(
    subscribeConversationStores,
    conversationRuntimeSnapshot,
    conversationRuntimeSnapshot,
  );

  const conversationRuntime = useMemo(() => {
    const entries = JSON.parse(conversationRuntimeSignature) as Array<
      [string, string | null, string | null, number | null]
    >;
    return new Map(
      entries.map(([id, runId, runStatus, latestResultSeq]) => [
        id,
        { runId, runStatus, latestWatchResultSeq: latestResultSeq },
      ]),
    );
  }, [conversationRuntimeSignature]);

  const waitingConversationIds = useMemo(
    () =>
      collectWaitingConversationIds(
        ensured,
        (conversationId) =>
          conversationRuntime.get(conversationId) ?? {
            runId: null,
            runStatus: null,
          },
      ),
    [ensured, conversationRuntime],
  );

  const unreadWatchConversationIds = useMemo(
    () =>
      collectUnreadWatchConversationIds(
        ensured,
        (conversationId) =>
          conversationRuntime.get(conversationId) ?? {
            runId: null,
            runStatus: null,
          },
      ),
    [ensured, conversationRuntime],
  );

  const boundHandoffsRef = useRef(new Set<string>());
  useEffect(() => {
    for (const conversation of ensured) {
      if (!conversation.workspaceHandoffId || !conversation.runId) continue;
      const key = `${conversation.workspaceHandoffId}:${conversation.runId}`;
      if (boundHandoffsRef.current.has(key)) continue;
      boundHandoffsRef.current.add(key);
      void bindWorkspaceHandoff(
        conversation.workspaceHandoffId,
        conversation.runId,
      ).catch((error) => {
        boundHandoffsRef.current.delete(key);
        addToast(`Could not bind the project handoff: ${errorMessage(error)}`, "error", {
          persistent: true,
        });
      });
    }
  }, [addToast, ensured]);

  useEffect(() => {
    postWorkspaceUnreadCount(unreadWatchConversationIds.size);
  }, [unreadWatchConversationIds]);

  // A watch result is read only once its own conversation is on screen.
  // Restoring a closed dock after a reboot must retain the proactive alert.
  useEffect(() => {
    if (!paneVisible || dock === "closed" || onNexieRoute || !active) return;
    const state = storeFor(active.id).getState();
    if (!active.runId || state.runId !== active.runId) return;
    const seq = latestWatchResultSeq(state.events);
    if (seq === null || seq <= (active.lastReadWatchResultSeq ?? -1)) return;
    persist((current) =>
      markConversationWatchResultRead(current, active.id, seq),
    );
  }, [
    active,
    conversationRuntimeSignature,
    dock,
    onNexieRoute,
    paneVisible,
    persist,
    storeFor,
  ]);

  const dockValue = useMemo<AssistantDockContextValue>(
    () => ({
      store: activeStore,
      dock,
      setDock,
      hasSession: activeBot !== null,
      dockWidth,
      setDockWidth,
      conversations: ensured,
      activeConversationId: active?.id ?? null,
      activeWorkspaceHandoffId: active?.workspaceHandoffId ?? null,
      anchorConversation: anchorConversationById,
      markContextUnknown,
      setConversationContextEnabled: setContextEnabled,
      openConversation,
      selectConversation,
      closeConversationById,
      closingConversationIds,
      waitingConversationIds,
      unreadWatchConversationIds,
      atConversationLimit: ensured.length >= MAX_CONVERSATIONS,
    }),
    [
      activeStore,
      dock,
      setDock,
      activeBot,
      dockWidth,
      setDockWidth,
      ensured,
      active,
      anchorConversationById,
      markContextUnknown,
      setContextEnabled,
      openConversation,
      selectConversation,
      closeConversationById,
      closingConversationIds,
      waitingConversationIds,
      unreadWatchConversationIds,
    ],
  );

  // Background conversations are mounted for one reason: to keep running while
  // the operator reads another. They render nothing.
  const background = ensured.filter((c) => c.id !== active?.id || onNexieRoute);

  return (
    <AssistantDockContext.Provider value={dockValue}>
      {disposalDialog}
      {background.map((c) => (
        <BackgroundConversation
          key={c.id}
          bot={registry.resolveDock(c.botId)}
          store={storeFor(c.id)}
          // A dock tab owns exactly its persisted run. Bot-scoped discovery
          // answers "latest run for this bot", which lets a legacy/seed tab
          // steal a neighbouring conversation and strands the previous run.
          discover={false}
          attachRunId={c.runId ?? null}
        />
      ))}
      <ActiveConversation
        // Keyed on the conversation AND the bot: switching either is a
        // different session, and a stale transcript must not bleed across.
        // The key lives on the session engine, NOT around {children} —
        // children is the authenticated app, and remounting it on a
        // /whats-next round-trip (or a bot hydrate) dropped scroll,
        // closed dialogs, and refetched every query.
        sessionKey={`${active?.id ?? "none"}:${activeBot?.id ?? "none"}`}
        bot={activeBot}
        bots={registry.dockBots}
        selectBot={selectBot}
        store={activeStore}
        discover={onNexieRoute}
        attachRunId={onNexieRoute ? null : active?.runId ?? null}
        onLaunched={onNexieRoute ? () => {} : markActiveLaunched}
        runSource={activeRunSource}
        beforeLaunch={onNexieRoute ? undefined : persistActiveBeforeLaunch}
        handoffPrompt={
          handoffPrompt && handoffPrompt.conversationId === active?.id
            ? handoffPrompt.message
            : null
        }
        onHandoffStarted={consumeHandoffPrompt}
      >
        {children}
      </ActiveConversation>
    </AssistantDockContext.Provider>
  );
}

// A conversation that is not on screen. It runs; it renders nothing.
function BackgroundConversation({
  bot,
  store,
  discover,
  attachRunId,
}: {
  bot: FirstClassBot | null;
  store: RunStore;
  discover: boolean;
  attachRunId: string | null;
}) {
  return (
    <RunStoreProvider store={store}>
      <RunSessionPump
        bot={bot ?? FALLBACK_BOT}
        discover={discover}
        attachRunId={attachRunId}
      />
    </RunStoreProvider>
  );
}

function RunSessionPump({
  bot,
  discover,
  attachRunId,
}: {
  bot: FirstClassBot;
  discover: boolean;
  attachRunId: string | null;
}) {
  useWhatsNextSession(bot, { discover, attachRunId });
  return null;
}

// The conversation on screen: it runs the session AND publishes it, so the
// dock and /whats-next read the same one.
function ActiveConversation({
  sessionKey,
  bot,
  bots,
  selectBot,
  store,
  discover,
  attachRunId,
  onLaunched,
  runSource,
  beforeLaunch,
  handoffPrompt,
  onHandoffStarted,
  children,
}: {
  sessionKey: string;
  bot: FirstClassBot | null;
  bots: FirstClassBot[];
  selectBot: (id: string) => void;
  store: RunStore;
  discover: boolean;
  attachRunId: string | null;
  onLaunched: (runId: string) => void;
  runSource?: NonNullable<WhatsNextSessionOptions["runSource"]>;
  beforeLaunch?: () => void;
  handoffPrompt: string | null;
  onHandoffStarted: () => void;
  children: ReactNode;
}) {
  const [published, setPublished] = useState<PublishedSession | null>(null);
  // Publishing a session updates this parent. Keep both the callback and the
  // engine stable so that update does not render the engine again solely
  // because its parent rendered: useWhatsNextSession returns a fresh facade,
  // which would otherwise publish again and form an infinite effect loop.
  // Internal engine updates still render it normally and publish the new
  // session value.
  const handleSession = useCallback(
    (key: string, value: AssistantSessionContextValue) => {
      setPublished((prev) =>
        prev &&
        prev.key === key &&
        prev.value.bot === value.bot &&
        prev.value.session === value.session &&
        prev.value.bots === value.bots &&
        prev.value.selectBot === value.selectBot
          ? prev
          : { key, value },
      );
    },
    [],
  );
  // The engine is remounted on a key change, but THIS state is not — and the
  // engine republishes from an effect, so without the key test the previous
  // conversation's transcript and run status stay on the context for at least
  // one commit while `activeConversationId` already names the new one. That
  // window is what let the migration effect read a foreign transcript and
  // either mark a fresh conversation `unknown` or, worse, silently anchor it
  // to the previous conversation's page. Derive rather than reset in an
  // effect: a reset would leave exactly the contaminated commit it removes.
  const sessionValue = published?.key === sessionKey ? published.value : null;
  return (
    <>
      <RunStoreProvider store={store}>
        <ActiveConversationEngine
          key={sessionKey}
          bot={bot}
          bots={bots}
          selectBot={selectBot}
          discover={discover}
          attachRunId={attachRunId}
          onLaunched={onLaunched}
          runSource={runSource}
          beforeLaunch={beforeLaunch}
          handoffPrompt={handoffPrompt}
          onHandoffStarted={onHandoffStarted}
          sessionKey={sessionKey}
          onSession={handleSession}
        />
      </RunStoreProvider>
      <AssistantSessionContext.Provider
        value={
          sessionValue ?? { bot, session: FALLBACK_SESSION, bots, selectBot }
        }
      >
        {/* Hand the default store back: everything below is the ordinary
            app, and must not read the assistant's run. Unkeyed so a
            session-key change cannot remount the route tree. */}
        <RunStoreProvider store={getDefaultRunStore()}>{children}</RunStoreProvider>
      </AssistantSessionContext.Provider>
    </>
  );
}

// Split so the session hook runs UNDER its conversation's store — the same
// reason AssistantSessionHost is split from AssistantProvider. Keyed by
// sessionKey in the parent; publishes the session as context rather than
// wrapping the app in the keyed node.
const ActiveConversationEngine = memo(function ActiveConversationEngine({
  bot,
  bots,
  selectBot,
  discover,
  attachRunId,
  onLaunched,
  runSource,
  beforeLaunch,
  handoffPrompt,
  onHandoffStarted,
  sessionKey,
  onSession,
}: {
  bot: FirstClassBot | null;
  bots: FirstClassBot[];
  selectBot: (id: string) => void;
  discover: boolean;
  attachRunId: string | null;
  onLaunched: (runId: string) => void;
  runSource?: NonNullable<WhatsNextSessionOptions["runSource"]>;
  beforeLaunch?: () => void;
  handoffPrompt: string | null;
  onHandoffStarted: () => void;
  // Stamped onto every publication so the parent can tell this engine's
  // session from the one it replaced.
  sessionKey: string;
  onSession: (key: string, value: AssistantSessionContextValue) => void;
}) {
  const session = useWhatsNextSession(bot ?? FALLBACK_BOT, {
    discover,
    attachRunId,
    runSource,
    beforeLaunch,
  });
  const recordedRef = useRef<string | null>(null);
  const handoffStartedRef = useRef(false);
  useEffect(() => {
    if (
      !handoffPrompt ||
      handoffStartedRef.current ||
      session.status !== "idle" ||
      !bot?.id
    ) {
      return;
    }
    handoffStartedRef.current = true;
    onHandoffStarted();
    // The session surfaces launch failures in its own error state. Consume the
    // rejected promise here so an unavailable backend does not become an
    // unhandled browser rejection as well.
    void session.launch({ initial_message: handoffPrompt }).catch(() => {});
  }, [bot?.id, handoffPrompt, onHandoffStarted, session]);
  useEffect(() => {
    if (!session.runId || recordedRef.current === session.runId) return;
    recordedRef.current = session.runId;
    onLaunched(session.runId);
  }, [session.runId, onLaunched]);
  const sessionValue = useMemo<AssistantSessionContextValue>(
    () => ({ bot, session, bots, selectBot }),
    [bot, session, bots, selectBot],
  );
  useEffect(() => {
    onSession(sessionKey, sessionValue);
  }, [sessionKey, sessionValue, onSession]);
  return null;
});

// AssistantStoreScope re-enters the assistant's run store. Any surface
// rendering the assistant's transcript or composer needs it, because
// those components (AgentChatboxInline, PreFlightPanel, …) read the run
// store from context and would otherwise see the default one.
export function AssistantStoreScope({ children }: { children: ReactNode }) {
  const ctx = useContext(AssistantDockContext);
  if (!ctx) return <>{children}</>;
  return <RunStoreProvider store={ctx.store}>{children}</RunStoreProvider>;
}

// Both hooks return null outside the provider so a surface can degrade
// (render nothing) instead of throwing — the provider is only mounted on
// the authenticated shell.
export function useAssistantDock(): AssistantDockContextValue | null {
  return useContext(AssistantDockContext);
}

export function useAssistantSession(): AssistantSessionContextValue | null {
  return useContext(AssistantSessionContext);
}

// How much of the right edge the assistant reserves in the LAYOUT, in px
// (0 unless it is docked on a route where it actually renders).
//
// AppShell reserves it as padding so the page is pushed aside rather than
// covered. Only the docked column earns that: a FLOATING panel is explicitly
// the mode that overlays without disturbing the page.
//
// Reads the DOCK context only: the session context changes on every
// websocket event and would re-render every consumer with it. That is
// why `hasSession` is mirrored onto the dock context — the condition
// below has to match ChatDock's own render guard exactly, or the shell
// reserves a column nothing fills.
export function useAssistantReservedWidthPx(): number {
  const ctx = useContext(AssistantDockContext);
  const [location] = useLocation();
  const reservesLayout = useWideDockViewport();
  if (!ctx?.hasSession) return 0;
  // On compact screens docked-right is an overlaying side sheet. Reserving
  // 380px there would squeeze the route to almost nothing; the sheet is
  // full-width-safe and can be minimised to reveal the untouched page.
  return ctx.dock === "docked-right" &&
    reservesLayout &&
    !dockStandsDown(location)
    ? clampDockWidth(ctx.dockWidth)
    : 0;
}

// Crossing the compact breakpoint changes docked-right from an overlaying
// side sheet to a real layout column. Keep AppShell subscribed so its padding
// changes at the same instant as the viewport instead of on the next route
// render.
function useWideDockViewport(): boolean {
  const read = () =>
    typeof window === "undefined" ? Number.POSITIVE_INFINITY : window.innerWidth;
  const [viewportWidth, setViewportWidth] = useState(read);
  useEffect(() => {
    const onResize = () => setViewportWidth(read());
    window.addEventListener("resize", onResize);
    return () => window.removeEventListener("resize", onResize);
  }, []);
  return viewportWidth > DOCK_BREAKPOINT_PX;
}

// How much of the right edge another FIXED bottom-right surface must clear.
//
// This is a different question from the layout reservation above, and
// conflating the two is what made the run console's steering bubble
// unclickable. Padding does nothing for a `fixed` element, so a peer surface
// has to step out of the assistant's band explicitly — and the assistant
// occupies a band in BOTH of its open states, not just the docked one.
//
// The floating case is the one that bit: the steering bubble sits at
// right:80 (lane 1) and the floating assistant spans right 16 → 436, so the
// bubble landed underneath it with the same z-index and the assistant, being
// mounted later, ate every click. The default dock state is `closed`, which
// is exactly the configuration an operator on /runs/:id is in — so "steering
// vs assistant is unambiguous" held in principle while steering was
// unreachable in practice.
export function useAssistantFixedInsetPx(): number {
  const ctx = useContext(AssistantDockContext);
  const [location] = useLocation();
  if (!ctx?.hasSession || dockStandsDown(location)) return 0;
  if (ctx.dock === "docked-right") return ctx.dockWidth;
  if (ctx.dock === "floating") return FLOATING_FOOTPRINT_PX;
  return 0;
}
