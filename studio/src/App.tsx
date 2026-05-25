import { Suspense, lazy, useEffect, useState } from "react";
import { Route, Switch } from "wouter";

import AppShell from "@/components/shared/AppShell";

// Routes are React.lazy'd so each view ships its own chunk and the
// initial download covers only the shell + AuthGate. The eager imports
// below are the always-needed shell pieces (Login lives off the auth
// gate; everything else is conditional on a route match).
const HomeView = lazy(() => import("@/components/Home/HomeView"));
const WhatsNextView = lazy(() => import("@/components/WhatsNext/WhatsNextView"));
const EditorTabsView = lazy(() => import("@/components/Editor/EditorTabsView"));
const LaunchView = lazy(() => import("@/components/Runs/LaunchView"));
const RunsTabsView = lazy(() => import("@/components/Runs/RunsTabsView"));
const BoardView = lazy(() => import("@/views/Board"));
const LabelsView = lazy(() => import("@/views/Board/Labels"));
const FindingsView = lazy(() => import("@/views/Findings"));
const RunsAnalyticsView = lazy(() => import("@/views/RunsAnalytics"));
const DispatcherView = lazy(() => import("@/views/Dispatcher"));
const Welcome = lazy(() => import("@/views/Welcome"));
const Settings = lazy(() => import("@/views/Settings"));
const ProjectSwitcher = lazy(() => import("@/views/ProjectSwitcher"));
const SettingsPage = lazy(() => import("@/views/settings/SettingsPage"));
const TeamPage = lazy(() => import("@/views/teams/TeamPage"));

import { ErrorBoundary } from "@/components/shared/ErrorBoundary";
import GlobalCommandPalette from "@/components/shared/GlobalCommandPalette";
import ToastContainer from "@/components/shared/Toast";
import MissingCLIBanner from "@/components/MissingCLIBanner";
import Login from "@/views/Login";
import { useDesktop } from "@/hooks/useDesktop";
import { useDocumentTitle } from "@/hooks/useDocumentTitle";
import { useProjectSwitchListener } from "@/hooks/useProjectSwitchListener";
import { onDesktopEvent } from "@/lib/desktopBridge";
import { DesktopEvent } from "@/lib/desktopEvents";
import { AuthProvider, useAuth } from "@/auth/AuthContext";
import { setUnauthorizedHandler } from "@/api/client";
import { getOrCreateDocumentStore } from "@/store/document";
import { useTabsStore } from "@/store/tabs";
import { useServerInfoStore } from "@/store/serverInfo";

// activeEditorDocStore looks up the document store for the editor
// tab currently shown in /editor. Returns null when no editor tab is
// open so menu undo/redo shortcuts silently no-op rather than
// mutating a stale global default.
function activeEditorDocStore() {
  const { activeEditorTabId } = useTabsStore.getState();
  if (!activeEditorTabId) return null;
  return getOrCreateDocumentStore(activeEditorTabId);
}

export default function App() {
  return (
    <AuthProvider>
      <AuthGate />
    </AuthProvider>
  );
}

// AuthGate decides between the Login view and the full editor based
// on the AuthProvider's status. It also wires the global 401
// interceptor so editor API calls bounce the user back to /login on
// session expiration.
function AuthGate() {
  const { status, signOut } = useAuth();

  useEffect(() => {
    setUnauthorizedHandler(() => {
      void signOut();
    });
    return () => setUnauthorizedHandler(null);
  }, [signOut]);

  if (status === "loading") {
    return (
      <div className="h-screen flex items-center justify-center bg-surface-0 text-fg-muted">
        Loading…
      </div>
    );
  }
  if (status === "anonymous") {
    return <Login />;
  }
  return <AuthedApp />;
}

function AuthedApp() {
  const { isDesktop, ready, firstRunPending, refresh, pickAndAddProject } =
    useDesktop();
  const serverInfo = useServerInfoStore((s) => s.info);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [settingsTab, setSettingsTab] = useState<string>("api-keys");
  const [switcherOpen, setSwitcherOpen] = useState(false);

  useDocumentTitle();
  // Reset SPA state on a server-side project hot-swap so the new
  // project's empty home view replaces whatever the user was looking
  // at. No-op in desktop (server-mode WS) and cloud modes.
  useProjectSwitchListener();

  useEffect(() => {
    const offs = [
      onDesktopEvent(DesktopEvent.MenuSettings, () => {
        setSettingsTab("api-keys");
        setSettingsOpen(true);
      }),
      onDesktopEvent(DesktopEvent.MenuSwitchProject, () => setSwitcherOpen(true)),
      // MenuNewProject opens the native directory picker directly —
      // previously it opened the switcher (same as MenuSwitchProject),
      // which forced users through an extra step. The picker is also
      // what the "+ Add project…" button inside the switcher uses.
      onDesktopEvent(DesktopEvent.MenuNewProject, () => {
        void pickAndAddProject();
      }),
      onDesktopEvent(DesktopEvent.MenuAbout, () => {
        setSettingsTab("about");
        setSettingsOpen(true);
      }),
      // Menu undo/redo route to the active editor tab's per-tab
      // document store. With multi-tab editors, a global singleton
      // would mutate the wrong document; the registry lookup keeps
      // the action scoped to whatever the user is looking at.
      onDesktopEvent(DesktopEvent.MenuUndo, () => activeEditorDocStore()?.getState().undo()),
      onDesktopEvent(DesktopEvent.MenuRedo, () => activeEditorDocStore()?.getState().redo()),
    ];
    // Listen for the SPA-emitted open-switcher event from ProjectLabel
    // (clicking the project chip in the toolbar / run header).
    const onOpenSwitcher = () => setSwitcherOpen(true);
    window.addEventListener("iterion:open-project-switcher", onOpenSwitcher);
    // Sidebar Settings button (and any other SPA caller) dispatches this
    // to surface the dialog. The optional `tab` detail lets callers
    // land on a specific section (Appearance, Backends, …).
    const onOpenSettings = (e: Event) => {
      const detail = (e as CustomEvent<{ tab?: string }>).detail;
      if (detail?.tab) setSettingsTab(detail.tab);
      else setSettingsTab(isDesktop ? "api-keys" : "appearance");
      setSettingsOpen(true);
    };
    window.addEventListener("iterion:open-settings", onOpenSettings as EventListener);
    return () => {
      offs.forEach((off) => off());
      window.removeEventListener("iterion:open-project-switcher", onOpenSwitcher);
      window.removeEventListener("iterion:open-settings", onOpenSettings as EventListener);
    };
  }, [pickAndAddProject, isDesktop]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "p") {
        e.preventDefault();
        setSwitcherOpen(true);
      }
      if ((e.metaKey || e.ctrlKey) && e.key === ",") {
        e.preventDefault();
        setSettingsTab("api-keys");
        setSettingsOpen(true);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  if (!ready) {
    return (
      <div className="h-screen bg-surface-0 text-fg-default p-8">Loading…</div>
    );
  }

  if (isDesktop && firstRunPending) {
    return (
      <Suspense
        fallback={
          <div className="h-screen bg-surface-0 text-fg-default p-8">Loading…</div>
        }
      >
        <Welcome onComplete={refresh} />
      </Suspense>
    );
  }

  return (
    <>
      {isDesktop && <MissingCLIBanner />}
      <AppShell>
        <Switch>
          <Route path="/runs/new">
            <ErrorBoundary area="Launch view">
              <LaunchView />
            </ErrorBoundary>
          </Route>
          <Route path="/runs/:id">
            <ErrorBoundary area="Run view">
              <RunsTabsView />
            </ErrorBoundary>
          </Route>
          <Route path="/runs">
            <ErrorBoundary area="Runs list">
              <RunsTabsView />
            </ErrorBoundary>
          </Route>
          <Route path="/account" component={SettingsPage} />
          <Route path="/teams/:id" component={TeamPage} />
          {serverInfo?.native_tracker_enabled && (
            <Route path="/board/labels">
              <ErrorBoundary area="Board labels view">
                <LabelsView />
              </ErrorBoundary>
            </Route>
          )}
          <Route path="/findings">
            <ErrorBoundary area="Findings view">
              <FindingsView />
            </ErrorBoundary>
          </Route>
          <Route path="/insights">
            <ErrorBoundary area="Runs analytics view">
              <RunsAnalyticsView />
            </ErrorBoundary>
          </Route>
          {serverInfo?.native_tracker_enabled && (
            <Route path="/board">
              <ErrorBoundary area="Board view">
                <BoardView />
              </ErrorBoundary>
            </Route>
          )}
          {serverInfo?.dispatcher_enabled && (
            <Route path="/dispatcher">
              <ErrorBoundary area="Dispatcher view">
                <DispatcherView />
              </ErrorBoundary>
            </Route>
          )}
          <Route path="/editor">
            <ErrorBoundary area="Editor view">
              <EditorTabsView />
            </ErrorBoundary>
          </Route>
          <Route path="/whats-next">
            <ErrorBoundary area="What's Next view">
              <WhatsNextView />
            </ErrorBoundary>
          </Route>
          <Route path="/" component={HomeView} />
          <Route component={HomeView} />
        </Switch>
      </AppShell>
      <ToastContainer />
      <GlobalCommandPalette />
      {/* Settings + ProjectSwitcher are also lazy and need their own
          Suspense boundary because they unmount/remount on open/close. */}
      <Suspense fallback={null}>
        <Settings
          open={settingsOpen}
          onClose={() => setSettingsOpen(false)}
          tab={settingsTab}
          onTabChange={setSettingsTab}
          desktopFeatures={isDesktop}
        />
        {/* ProjectSwitcher renders in both desktop and local-server modes.
            Cloud mode (no work_dir) renders nothing useful; we still mount
            it so the Ctrl+P shortcut and ProjectLabel chip have somewhere
            to dispatch — the dialog just shows an empty list there. */}
        {serverInfo?.mode !== "cloud" && (
          <ProjectSwitcher open={switcherOpen} onClose={() => setSwitcherOpen(false)} />
        )}
      </Suspense>
    </>
  );
}

