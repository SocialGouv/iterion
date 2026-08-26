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
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
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
  referenceForRoute,
} from "@/lib/chatDock/routeReference";
import { cancelRun, type RunStatus } from "@/api/runs";
import {
  MAX_CONVERSATIONS,
  addConversation,
  claimRun,
  closeConversation,
  newConversationId,
  readActiveConversation,
  readConversations,
  resolveActive,
  writeActiveConversation,
  writeConversations,
  type Conversation,
} from "@/lib/chatDock/conversations";
import {
  ASSISTANT_BOT_KEY,
  ASSISTANT_DOCK_KEY,
  DOCK_BREAKPOINT_PX,
  DOCKED_WIDTH_DEFAULT_PX,
  clampDockWidth,
  readDockState,
  readDockWidth,
  writeDockState,
  writeDockWidth,
  type DockState,
} from "@/lib/chatDock/dockState";
import { readStringFlag, writeStringFlag } from "@/lib/localStorageFlag";
import { useChatRegistry } from "@/hooks/useChatRegistry";
import { DEFAULT_WHATS_NEXT_BOT_ID } from "@/lib/whats-next/firstClassBots";
import {
  type FirstClassBot,
} from "@/lib/whats-next/firstClassBots";
import {
  useWhatsNextSession,
  type UseWhatsNextSession,
} from "@/lib/whats-next/useWhatsNextSession";
import {
  createRunStore,
  getDefaultRunStore,
  RunStoreProvider,
  type RunStore,
} from "@/store/run";

interface AssistantDockContextValue {
  store: RunStore;
  dock: DockState;
  setDock: (next: DockState) => void;
  // The page reference the operator dismissed, if any. Lives here rather
  // than in the dock because the dock UNMOUNTS on /whats-next (the route
  // renders the session itself), and a dismissal that came back on the
  // round trip would be a promise broken exactly where the operator
  // stops watching for it. Like the session, it is per-user state the
  // route tree only borrows.
  dismissedRef: string | null;
  setDismissedRef: (ref: string | null) => void;
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
  openConversation: () => void;
  selectConversation: (id: string) => void;
  closeConversationById: (id: string) => void;
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

export function AssistantProvider({ children }: { children: ReactNode }) {
  // One store for the whole app lifetime. Not the registry
  // (getOrCreateRunStore) — that is keyed by runId, and the assistant's
  // runId is only known after discovery.
  const store = useMemo(() => createRunStore(), []);
  const [location] = useLocation();
  return (
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

  const [conversations, setConversations] = useState<Conversation[]>(() =>
    readConversations(),
  );
  const [activeId, setActiveId] = useState<string>(() =>
    readActiveConversation(),
  );

  // One run store PER conversation, kept for the host's lifetime.
  //
  // Not one shared store: the session hook resets on a bot change, not on a
  // conversation change, so two conversations with the same bot would show
  // each other's run. Keeping them apart is also what lets a background
  // conversation still be there — transcript and all — when you come back.
  const storesRef = useRef<Map<string, RunStore>>(new Map());
  const storeFor = useCallback((id: string): RunStore => {
    const existing = storesRef.current.get(id);
    if (existing) return existing;
    const made = createRunStore();
    storesRef.current.set(id, made);
    return made;
  }, []);

  const persist = useCallback((list: Conversation[], active: string | null) => {
    setConversations(list);
    writeConversations(list);
    setActiveId(active ?? "");
    writeActiveConversation(active ?? "");
  }, []);

  // The dock always has at least one conversation to show. Created lazily, so
  // a browser that never opens the dock never persists one.
  const ensured = useMemo(() => {
    if (conversations.length > 0) return conversations;
    const seed: Conversation = {
      id: newConversationId(),
      botId: readStringFlag(ASSISTANT_BOT_KEY, ""),
    };
    return [seed];
  }, [conversations]);

  const active = resolveActive(ensured, activeId);

  const openConversation = useCallback(() => {
    const next = addConversation(ensured, {
      id: newConversationId(),
      botId: active?.botId ?? "",
      fresh: true,
      ...currentOrigin(location),
    });
    if (next.length === ensured.length) return; // at the ceiling
    persist(next, next[next.length - 1]!.id);
  }, [ensured, active, location, persist]);

  const selectConversation = useCallback(
    (id: string) => persist(ensured, id),
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
      const next = claimRun(ensured, active.id, runId);
      if (!next) return; // already another conversation's run
      persist(next, active.id);
    },
    [active, ensured, persist],
  );

  const closeConversationById = useCallback(
    (id: string) => {
      // Closing a tab must CANCEL its run, not just forget it. A conversation
      // is a live agent: dropped without cancelling, it keeps burning model
      // spend until a stall watchdog or a process restart tears it down, and
      // nothing on screen would ever mention it again. Same rule the
      // new-session action follows.
      const snapshot = storesRef.current.get(id)?.getState().snapshot;
      const runId = snapshot?.run.id;
      const status = snapshot?.run.status;
      if (runId && status && LIVE_RUN_STATUSES.has(status)) {
        // Best effort: a cancel that races a run finishing on its own must not
        // keep the tab open. The worst case is a quiescent run the existing
        // sweep reconciles.
        void cancelRun(runId).catch(() => {});
      }
      storesRef.current.delete(id);
      const got = closeConversation(ensured, id, active?.id ?? "");
      persist(got.list, got.activeId);
    },
    [ensured, active, persist, storesRef],
  );

  // Two lanes, still. Nexie owns /whats-next and answers there whatever the
  // dock's strip holds, so that route runs its OWN conversation rather than
  // taking over one of the operator's.
  const onNexieRoute = isAssistantOwnRoute(location);
  const nexieBot = registry.byId[DEFAULT_WHATS_NEXT_BOT_ID] ?? null;

  const selectBot = useCallback(
    (id: string) => {
      writeStringFlag(ASSISTANT_BOT_KEY, id);
      if (!active) return;
      persist(
        ensured.map((c) => (c.id === active.id ? { ...c, botId: id } : c)),
        active.id,
      );
    },
    [active, ensured, persist],
  );

  const [dock, setDockState] = useState<DockState>(() =>
    readDockState(ASSISTANT_DOCK_KEY, "closed"),
  );
  const setDock = useCallback((next: DockState) => {
    setDockState(next);
    writeDockState(ASSISTANT_DOCK_KEY, next);
  }, []);

  const [dockWidth, setDockWidthState] = useState<number>(() =>
    readDockWidth(DOCKED_WIDTH_DEFAULT_PX),
  );
  const setDockWidth = useCallback((px: number) => {
    const next = clampDockWidth(px);
    setDockWidthState(next);
    writeDockWidth(next);
  }, []);

  const [dismissedRef, setDismissedRef] = useState<string | null>(null);

  const activeBot = onNexieRoute
    ? nexieBot
    : registry.resolveDock(active?.botId ?? "");

  // /whats-next runs Nexie's own conversation, so it gets its own store too
  // rather than borrowing whichever dock tab happens to be active.
  const activeStore = onNexieRoute
    ? storeFor("__whats-next")
    : storeFor(active?.id ?? "__none");

  const dockValue = useMemo<AssistantDockContextValue>(
    () => ({
      store: activeStore,
      dock,
      setDock,
      dismissedRef,
      setDismissedRef,
      hasSession: activeBot !== null,
      dockWidth,
      setDockWidth,
      conversations: ensured,
      activeConversationId: active?.id ?? null,
      openConversation,
      selectConversation,
      closeConversationById,
      atConversationLimit: ensured.length >= MAX_CONVERSATIONS,
    }),
    [
      activeStore,
      dock,
      setDock,
      dismissedRef,
      activeBot,
      dockWidth,
      setDockWidth,
      ensured,
      active,
      openConversation,
      selectConversation,
      closeConversationById,
    ],
  );

  // Background conversations are mounted for one reason: to keep running while
  // the operator reads another. They render nothing.
  const background = ensured.filter((c) => c.id !== active?.id || onNexieRoute);

  return (
    <AssistantDockContext.Provider value={dockValue}>
      {background.map((c) => (
        <BackgroundConversation
          key={c.id}
          bot={registry.resolveDock(c.botId)}
          store={storeFor(c.id)}
          discover={!c.fresh && !c.runId}
          attachRunId={c.runId ?? null}
        />
      ))}
      <ActiveConversation
        // Keyed on the conversation AND the bot: switching either is a
        // different session, and a stale transcript must not bleed across.
        key={`${active?.id ?? "none"}:${activeBot?.id ?? "none"}`}
        bot={activeBot}
        bots={registry.dockBots}
        selectBot={selectBot}
        store={activeStore}
        discover={onNexieRoute ? true : !active?.fresh && !active?.runId}
        attachRunId={onNexieRoute ? null : active?.runId ?? null}
        onLaunched={onNexieRoute ? () => {} : markActiveLaunched}
      >
        {children}
      </ActiveConversation>
    </AssistantDockContext.Provider>
  );
}

// The statuses where a run is still consuming something. `paused_*` included:
// a paused run holds its worktree and its place in the concurrency budget, and
// the operator closing the tab is saying they are done with it.
const LIVE_RUN_STATUSES = new Set<RunStatus>([
  "queued",
  "running",
  "paused_waiting_human",
  "paused_operator",
]);

// currentOrigin captures WHERE a conversation was opened from, so the operator
// can get back to what they were talking about after switching tabs. The
// reverse of the page context the dock already sends the bot.
function currentOrigin(path: string): Pick<Conversation, "origin" | "originLabel"> {
  const ref = referenceForRoute(path, "");
  if (!ref) return {};
  return { origin: ref.ref, originLabel: ref.label };
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
  bot,
  bots,
  selectBot,
  store,
  discover,
  attachRunId,
  onLaunched,
  children,
}: {
  bot: FirstClassBot | null;
  bots: FirstClassBot[];
  selectBot: (id: string) => void;
  store: RunStore;
  discover: boolean;
  attachRunId: string | null;
  onLaunched: (runId: string) => void;
  children: ReactNode;
}) {
  return (
    <RunStoreProvider store={store}>
      <ActiveConversationInner
        bot={bot}
        bots={bots}
        selectBot={selectBot}
        discover={discover}
        attachRunId={attachRunId}
        onLaunched={onLaunched}
      >
        {children}
      </ActiveConversationInner>
    </RunStoreProvider>
  );
}

// Split so the session hook runs UNDER its conversation's store — the same
// reason AssistantSessionHost is split from AssistantProvider.
function ActiveConversationInner({
  bot,
  bots,
  selectBot,
  discover,
  attachRunId,
  onLaunched,
  children,
}: {
  bot: FirstClassBot | null;
  bots: FirstClassBot[];
  selectBot: (id: string) => void;
  discover: boolean;
  attachRunId: string | null;
  onLaunched: (runId: string) => void;
  children: ReactNode;
}) {
  const session = useWhatsNextSession(bot ?? FALLBACK_BOT, {
    discover,
    attachRunId,
  });
  const recordedRef = useRef<string | null>(null);
  useEffect(() => {
    if (!session.runId || recordedRef.current === session.runId) return;
    recordedRef.current = session.runId;
    onLaunched(session.runId);
  }, [session.runId, onLaunched]);
  const sessionValue = useMemo<AssistantSessionContextValue>(
    () => ({ bot, session, bots, selectBot }),
    [bot, session, bots, selectBot],
  );
  return (
    <AssistantSessionContext.Provider value={sessionValue}>
      {/* Hand the default store back: everything below is the ordinary
          app, and must not read the assistant's run. */}
      <RunStoreProvider store={getDefaultRunStore()}>
        {children}
      </RunStoreProvider>
    </AssistantSessionContext.Provider>
  );
}

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
    ? ctx.dockWidth
    : 0;
}

// Crossing the compact breakpoint changes docked-right from an overlaying
// side sheet to a real layout column. Keep AppShell subscribed so its padding
// changes at the same instant as the viewport instead of on the next route
// render.
function useWideDockViewport(): boolean {
  const read = () =>
    typeof window === "undefined" || window.innerWidth > DOCK_BREAKPOINT_PX;
  const [wide, setWide] = useState(read);
  useEffect(() => {
    const onResize = () => setWide(read());
    window.addEventListener("resize", onResize);
    return () => window.removeEventListener("resize", onResize);
  }, []);
  return wide;
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
