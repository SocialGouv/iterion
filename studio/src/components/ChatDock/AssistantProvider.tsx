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
// Dock state lives here too, and is persisted per USER rather than per
// route: docking the assistant on /board must leave it docked on /runs.

import {
  createContext,
  useCallback,
  useContext,
  useMemo,
  useState,
  type ReactNode,
} from "react";

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

interface AssistantContextValue {
  // Null only if the bot registry lookup misses (it can't today —
  // DEFAULT_WHATS_NEXT_BOT_ID is a const key — but the surfaces below
  // must degrade rather than crash if the registry becomes dynamic,
  // which is exactly what the manifest-driven registry work will do).
  bot: FirstClassBot | null;
  session: UseWhatsNextSession;
  store: RunStore;
  dock: DockState;
  setDock: (next: DockState) => void;
}

const AssistantContext = createContext<AssistantContextValue | null>(null);

// The registry lookup happens once at module scope: the fallback bot
// keeps hook order valid on a miss (hooks must run unconditionally).
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

  const value = useMemo<AssistantContextValue>(
    () => ({ bot, session, store, dock, setDock }),
    [bot, session, store, dock, setDock],
  );

  return (
    <AssistantContext.Provider value={value}>
      {/* Hand the default store back: everything below is the ordinary
          app, and must not read the assistant's run. */}
      <RunStoreProvider store={getDefaultRunStore()}>{children}</RunStoreProvider>
    </AssistantContext.Provider>
  );
}

// AssistantStoreScope re-enters the assistant's run store. Any surface
// rendering the assistant's transcript or composer needs it, because
// those components (AgentChatboxInline, PreFlightPanel, …) read the run
// store from context and would otherwise see the default one.
export function AssistantStoreScope({ children }: { children: ReactNode }) {
  const ctx = useContext(AssistantContext);
  if (!ctx) return <>{children}</>;
  return <RunStoreProvider store={ctx.store}>{children}</RunStoreProvider>;
}

// useAssistant returns null outside the provider so a surface can degrade
// (render nothing) instead of throwing — the provider is only mounted on
// the authenticated shell.
export function useAssistant(): AssistantContextValue | null {
  return useContext(AssistantContext);
}
