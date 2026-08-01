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
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { useLocation } from "wouter";

import { DOCKED_WIDTH_PX } from "@/components/ChatDock/ChatDockShell";
import { isAssistantOwnRoute } from "@/lib/chatDock/routeReference";
import {
  ASSISTANT_DOCK_KEY,
  readDockState,
  writeDockState,
  type DockState,
} from "@/lib/chatDock/dockState";
import {
  DEFAULT_WHATS_NEXT_BOT_ID,
  getFirstClassBot,
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
}

interface AssistantSessionContextValue {
  // Null only if the bot registry lookup misses. It can't today —
  // DEFAULT_WHATS_NEXT_BOT_ID is a const key — but the surfaces below
  // must degrade rather than crash once that registry becomes
  // manifest-driven and therefore dynamic.
  bot: FirstClassBot | null;
  session: UseWhatsNextSession;
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
  return (
    <RunStoreProvider store={store}>
      <AssistantSessionHost store={store}>{children}</AssistantSessionHost>
    </RunStoreProvider>
  );
}

// Runs the session hook — which must execute UNDER the assistant store,
// hence the split from AssistantProvider.
function AssistantSessionHost({
  store,
  children,
}: {
  store: RunStore;
  children: ReactNode;
}) {
  const bot = getFirstClassBot(DEFAULT_WHATS_NEXT_BOT_ID);
  const session = useWhatsNextSession(bot ?? FALLBACK_BOT);

  const [dock, setDockState] = useState<DockState>(() =>
    readDockState(ASSISTANT_DOCK_KEY, "closed"),
  );
  const setDock = useCallback((next: DockState) => {
    setDockState(next);
    writeDockState(ASSISTANT_DOCK_KEY, next);
  }, []);

  const [dismissedRef, setDismissedRef] = useState<string | null>(null);

  const dockValue = useMemo<AssistantDockContextValue>(
    () => ({ store, dock, setDock, dismissedRef, setDismissedRef }),
    [store, dock, setDock, dismissedRef],
  );
  const sessionValue = useMemo<AssistantSessionContextValue>(
    () => ({ bot, session }),
    [bot, session],
  );

  return (
    <AssistantDockContext.Provider value={dockValue}>
      <AssistantSessionContext.Provider value={sessionValue}>
        {/* Hand the default store back: everything below is the ordinary
            app, and must not read the assistant's run. */}
        <RunStoreProvider store={getDefaultRunStore()}>
          {children}
        </RunStoreProvider>
      </AssistantSessionContext.Provider>
    </AssistantDockContext.Provider>
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

// How much of the right edge the assistant currently occupies, in px
// (0 unless it is docked on a route where it actually renders).
//
// Two kinds of consumer, and they need it for opposite reasons:
//   - AppShell reserves it as padding, so the page is pushed aside
//     rather than covered;
//   - any OTHER fixed bottom-right surface — the run console's steering
//     bubble — offsets by it, because padding does nothing for a
//     `fixed` element and the bubble would otherwise sit UNDER the
//     assistant's column, unclickable.
// Reads the DOCK context only: the session context changes on every
// websocket event and would re-render every consumer with it.
export function useAssistantReservedWidthPx(): number {
  const ctx = useContext(AssistantDockContext);
  const [location] = useLocation();
  return ctx?.dock === "docked-right" && !isAssistantOwnRoute(location)
    ? DOCKED_WIDTH_PX
    : 0;
}
