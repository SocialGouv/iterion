import { Suspense, lazy, useEffect, useState } from "react";
import { Route, Switch, useLocation } from "wouter";

import AppShell from "@/components/shared/AppShell";
import BootLoading from "@/components/shared/BootLoading";
import ServerUnreachable from "@/components/shared/ServerUnreachable";

// Routes are React.lazy'd so each view ships its own chunk and the
// initial download covers only the shell + AuthGate. The eager imports
// below are the always-needed shell pieces (Login lives off the auth
// gate; everything else is conditional on a route match).
const HomeView = lazy(() => import("@/components/Home/HomeView"));
const WhatsNextView = lazy(() => import("@/components/WhatsNext/WhatsNextView"));
const EditorTabsView = lazy(() => import("@/components/Editor/EditorTabsView"));
const LaunchView = lazy(() => import("@/components/Runs/LaunchView"));
const RunsTabsView = lazy(() => import("@/components/Runs/RunsTabsView"));
const BotsView = lazy(() => import("@/views/Bots"));
const BotHomeView = lazy(() => import("@/views/Bots/BotHome"));
const BotBuilderView = lazy(() => import("@/views/Bots/BotBuilder"));
const BoardView = lazy(() => import("@/views/Board"));
const PipelineBoardView = lazy(() => import("@/views/PipelineBoard"));
const PipelineCardPage = lazy(() => import("@/views/PipelineBoard/PipelineCardPage"));
const LabelsView = lazy(() => import("@/views/Board/Labels"));
const FieldsView = lazy(() => import("@/views/Board/Fields"));
const RunsAnalyticsView = lazy(() => import("@/views/RunsAnalytics"));
const DispatcherView = lazy(() => import("@/views/Dispatcher"));
const TriggersView = lazy(() => import("@/views/Triggers"));
const MarketplaceView = lazy(() => import("@/views/Marketplace"));
const PluginsView = lazy(() => import("@/views/Plugins"));
const SecretsView = lazy(() => import("@/views/Secrets"));
const SkillsView = lazy(() => import("@/views/Skills"));
const OrgsAdminPage = lazy(() => import("@/views/admin/OrgsAdminPage"));
const UsersAdminPage = lazy(() => import("@/views/admin/UsersAdminPage"));
const Welcome = lazy(() => import("@/views/Welcome"));
const SettingsDialog = lazy(() => import("@/views/SettingsDialog"));
const ProjectSwitcher = lazy(() => import("@/views/ProjectSwitcher"));
const CloudReloginModal = lazy(() => import("@/components/shared/CloudReloginModal"));
const SettingsPage = lazy(() => import("@/views/account/SettingsPage"));
const TeamPage = lazy(() => import("@/views/teams/TeamPage"));
const IntegrationsPage = lazy(() => import("@/views/integrations/IntegrationsPage"));
const OrgPage = lazy(() => import("@/views/orgs/OrgPage"));

// Auth side-doors reachable when anonymous (forced password rotation,
// forgot/reset password) and when authed (invitation accept).
const ForcedPasswordChange = lazy(() => import("@/views/auth/ForcedPasswordChange"));
const ForgotPassword = lazy(() => import("@/views/auth/ForgotPassword"));
const ResetPassword = lazy(() => import("@/views/auth/ResetPassword"));
const AcceptInvitation = lazy(() => import("@/views/auth/AcceptInvitation"));

import { ErrorBoundary } from "@/components/shared/ErrorBoundary";
import GlobalCommandPalette from "@/components/shared/GlobalCommandPalette";
import ToastContainer from "@/components/shared/Toast";
import MissingCLIBanner from "@/components/MissingCLIBanner";
import CloudLanding, { PublicTopBar } from "@/views/CloudLanding";
const RestrictedShell = lazy(() => import("@/views/RestrictedShell"));
const CliAuthPage = lazy(() => import("@/views/CliAuthPage"));
import { useDesktop } from "@/hooks/useDesktop";
import { useDocumentTitle } from "@/hooks/useDocumentTitle";
import { useProjectSwitchListener } from "@/hooks/useProjectSwitchListener";
import { useProjectScopeSync } from "@/hooks/useProjectScopeSync";
import { onDesktopEvent } from "@/lib/desktopBridge";
import { DesktopEvent } from "@/lib/desktopEvents";
import { isScopedPane, scopePrefix } from "@/lib/scope";
import { showRunAlertNotification, type RunAlertPayload } from "@/lib/desktopNotify";
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

// ScopedPaneReauth is what a workspace pane shows when its cloud session
// expired: it signals the shell (parent frame) to prompt re-login — the pane
// can't (no window.go) — and waits for the shell to re-arm the token + reload.
function ScopedPaneReauth() {
  useEffect(() => {
    const connId = scopePrefix().replace(/^\/x\//, "");
    window.parent?.postMessage({ source: "iterion-pane", type: "auth-expired", connId }, "*");
  }, []);
  return (
    <div className="h-screen flex flex-col items-center justify-center gap-2 bg-surface-0 text-fg-muted px-6 text-center">
      <p className="text-sm font-medium text-fg-default">Session expired</p>
      <p className="text-xs">Reconnect from the desktop — the sign-in prompt should appear.</p>
    </div>
  );
}

// AuthGate decides between the Login view and the full editor based
// on the AuthProvider's status. It also wires the global 401
// interceptor so editor API calls bounce the user back to /login on
// session expiration.
//
// Side-doors: a small set of paths (forgot-password, reset, forced
// password change, invitation accept) must be reachable WITHOUT a
// session so the AuthGate consults the URL when it sees the
// "anonymous" state and dispatches to the matching public view.
function AuthGate() {
  const { status, signOut, isRestricted, retryConnection } = useAuth();
  const [location] = useLocation();
  const serverInfo = useServerInfoStore((s) => s.info);

  useEffect(() => {
    setUnauthorizedHandler(() => {
      void signOut();
    });
    return () => setUnauthorizedHandler(null);
  }, [signOut]);

  if (status === "loading") {
    return <BootLoading />;
  }
  // Backend down ≠ signed out: show the reconnect screen, never the
  // sign-in form (a local no-auth operator has no credentials to give).
  if (status === "unreachable") {
    return <ServerUnreachable onRetry={retryConnection} />;
  }
  if (status === "anonymous") {
    // A workspace pane (iframe) can't run its own auth — the desktop owns the
    // token jar. On expiry, ask the shell (parent frame) to prompt re-login,
    // then reload us; don't render the pane's own Login/landing.
    if (isScopedPane()) {
      return <ScopedPaneReauth />;
    }
    return (
      <Suspense fallback={<BootLoading />}>
        <Switch>
          <Route path="/auth/password/change" component={ForcedPasswordChange} />
          <Route path="/auth/forgot-password" component={ForgotPassword} />
          <Route path="/auth/reset" component={ResetPassword} />
          <Route path="/invitations/accept" component={AcceptInvitation} />
          {/* Public marketplace — browsable + downloadable without an
              account (submit/install gate behind sign-in). Outside the
              AppShell, so it carries its own slim top bar. */}
          {serverInfo?.marketplace_enabled && (
            <Route path="/marketplace">
              <div className="min-h-screen bg-surface-0 text-fg-default">
                <PublicTopBar />
                <ErrorBoundary area="Marketplace view">
                  <MarketplaceView />
                </ErrorBoundary>
              </div>
            </Route>
          )}
          {/* Catch-all: the cloud marketing landing (hero + sign-in card),
              degrading to the plain sign-in page in non-cloud modes. */}
          <Route component={CloudLanding} />
        </Switch>
      </Suspense>
    );
  }
  // Authenticated paths that don't belong in the AppShell go here (the
  // invitation accept needs the AuthContext but not the full shell). Kept
  // reachable for restricted users so they can accept a team invitation.
  if (location.startsWith("/invitations/accept")) {
    return (
      <Suspense fallback={<BootLoading />}>
        <AcceptInvitation />
      </Suspense>
    );
  }
  // Browser half of `iterion remote login` — approve + mint a CLI token. Any
  // authenticated user (incl. the restricted tier) can authorize the CLI.
  if (location.startsWith("/cli-auth")) {
    return (
      <Suspense fallback={<BootLoading />}>
        <CliAuthPage />
      </Suspense>
    );
  }
  // The "submitter" tier (signed in, no team, not super-admin) gets a
  // marketplace-only shell instead of the full studio.
  if (isRestricted) {
    return (
      <Suspense fallback={<BootLoading />}>
        <RestrictedShell />
      </Suspense>
    );
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
  // Set to a cloud connection id when its session expires (cloud:auth-expired);
  // opens the re-login modal.
  const [cloudReloginConnId, setCloudReloginConnId] = useState<string | null>(null);

  useDocumentTitle();
  // Reset SPA state on a server-side project hot-swap so the new
  // project's empty home view replaces whatever the user was looking
  // at. No-op in desktop (server-mode WS) and cloud modes.
  useProjectSwitchListener();
  // Scope run/editor tabs to the active project (hide other projects'
  // tabs instead of leaking them across a switch).
  useProjectScopeSync();

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
      // Native OS notification for run-health alerts. No-op in browser
      // mode (onDesktopEvent returns a noop unsubscribe there).
      onDesktopEvent<RunAlertPayload>(DesktopEvent.RunAlert, (payload) =>
        showRunAlertNotification(payload),
      ),
      // A cloud connection's session expired (silent refresh rejected) —
      // prompt re-login for that connection. Payload is the connection id.
      onDesktopEvent<string>(DesktopEvent.CloudAuthExpired, (connId) =>
        setCloudReloginConnId(connId),
      ),
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
    // In a workspace pane, native menu events reach the main frame, not this
    // iframe — the shell FORWARDS the connection-scoped ones (Settings / About /
    // Undo / Redo) via postMessage. Translate them into the same local actions.
    const onShellMenu = (e: MessageEvent) => {
      const d = e.data as { source?: string; type?: string; menu?: string; tab?: string } | null;
      if (d?.source !== "iterion-shell" || d.type !== "menu") return;
      switch (d.menu) {
        case "settings":
          setSettingsTab(d.tab || (isDesktop ? "api-keys" : "appearance"));
          setSettingsOpen(true);
          break;
        case "undo":
          activeEditorDocStore()?.getState().undo();
          break;
        case "redo":
          activeEditorDocStore()?.getState().redo();
          break;
      }
    };
    window.addEventListener("message", onShellMenu);
    return () => {
      offs.forEach((off) => off());
      window.removeEventListener("iterion:open-project-switcher", onOpenSwitcher);
      window.removeEventListener("iterion:open-settings", onOpenSettings as EventListener);
      window.removeEventListener("message", onShellMenu);
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
      <BootLoading />
    );
  }

  if (isDesktop && firstRunPending) {
    return (
      <Suspense
        fallback={
          <BootLoading />
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
          {/* Order matters: the literal /bots/new must win over /bots/:name. */}
          <Route path="/bots/new">
            <ErrorBoundary area="Bot builder view">
              <BotBuilderView />
            </ErrorBoundary>
          </Route>
          <Route path="/bots/:name">
            <ErrorBoundary area="Bot home view">
              <BotHomeView />
            </ErrorBoundary>
          </Route>
          <Route path="/bots">
            <ErrorBoundary area="Bots view">
              <BotsView />
            </ErrorBoundary>
          </Route>
          <Route path="/account" component={SettingsPage} />
          <Route path="/orgs/:id" component={OrgPage} />
          <Route path="/teams/:id" component={TeamPage} />
          <Route path="/integrations" component={IntegrationsPage} />
          <Route path="/admin" component={OrgsAdminPage} />
          <Route path="/admin/orgs" component={OrgsAdminPage} />
          <Route path="/admin/users" component={UsersAdminPage} />
          {serverInfo?.native_tracker_enabled && (
            <Route path="/board/labels">
              <ErrorBoundary area="Board labels view">
                <LabelsView />
              </ErrorBoundary>
            </Route>
          )}
          {serverInfo?.native_tracker_enabled && (
            <Route path="/board/fields">
              <ErrorBoundary area="Board fields view">
                <FieldsView />
              </ErrorBoundary>
            </Route>
          )}
          <Route path="/insights">
            <ErrorBoundary area="Runs analytics view">
              <RunsAnalyticsView />
            </ErrorBoundary>
          </Route>
          {serverInfo?.plugins_enabled && (
            <Route path="/plugins">
              <ErrorBoundary area="Plugins view">
                <PluginsView />
              </ErrorBoundary>
            </Route>
          )}
          {serverInfo?.secrets_enabled && (
            <Route path="/secrets">
              <ErrorBoundary area="Secrets view">
                <SecretsView />
              </ErrorBoundary>
            </Route>
          )}
          {serverInfo?.skills_enabled && (
            <Route path="/skills">
              <ErrorBoundary area="Skills view">
                <SkillsView />
              </ErrorBoundary>
            </Route>
          )}
          {serverInfo?.native_tracker_enabled && (
            <Route path="/pipelines/cards/:kind/:id">
              <ErrorBoundary area="Pipeline card page">
                <PipelineCardPage />
              </ErrorBoundary>
            </Route>
          )}
          {serverInfo?.native_tracker_enabled && (
            <Route path="/pipelines">
              <ErrorBoundary area="Pipeline board view">
                <PipelineBoardView />
              </ErrorBoundary>
            </Route>
          )}
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
          {serverInfo?.triggers_enabled && (
            <Route path="/triggers">
              <ErrorBoundary area="Automations view">
                <TriggersView />
              </ErrorBoundary>
            </Route>
          )}
          {serverInfo?.marketplace_enabled && (
            <Route path="/marketplace">
              <ErrorBoundary area="Marketplace view">
                <MarketplaceView />
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
        <SettingsDialog
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
        <CloudReloginModal
          connId={cloudReloginConnId}
          onClose={() => setCloudReloginConnId(null)}
        />
      </Suspense>
    </>
  );
}
