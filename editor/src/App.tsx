import { useEffect, useState } from "react";
import { Route, Switch, useLocation } from "wouter";

import EditorView from "@/components/EditorView";
import LaunchView from "@/components/Runs/LaunchView";
import RunListView from "@/components/Runs/RunListView";
import RunView from "@/components/Runs/RunView";
import ToastContainer from "@/components/shared/Toast";
import MissingCLIBanner from "@/components/MissingCLIBanner";
import Welcome from "@/views/Welcome";
import Settings from "@/views/Settings";
import ProjectSwitcher from "@/views/ProjectSwitcher";
import Login from "@/views/Login";
import SettingsPage from "@/views/settings/SettingsPage";
import TeamPage from "@/views/teams/TeamPage";
import { useDesktop } from "@/hooks/useDesktop";
import { onDesktopEvent } from "@/lib/desktopBridge";
import { DesktopEvent } from "@/lib/desktopEvents";
import { AuthProvider, useAuth } from "@/auth/AuthContext";
import { setUnauthorizedHandler } from "@/api/client";

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
  const { isDesktop, ready, firstRunPending, refresh } = useDesktop();
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [switcherOpen, setSwitcherOpen] = useState(false);

  useEffect(() => {
    const offs = [
      onDesktopEvent(DesktopEvent.MenuSettings, () => setSettingsOpen(true)),
      onDesktopEvent(DesktopEvent.MenuSwitchProject, () => setSwitcherOpen(true)),
      onDesktopEvent(DesktopEvent.MenuOpenProject, () => setSwitcherOpen(true)),
      onDesktopEvent(DesktopEvent.MenuNewProject, () => setSwitcherOpen(true)),
      onDesktopEvent(DesktopEvent.MenuAbout, () => setSettingsOpen(true)),
    ];
    return () => offs.forEach((off) => off());
  }, []);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "p") {
        e.preventDefault();
        setSwitcherOpen(true);
      }
      if ((e.metaKey || e.ctrlKey) && e.key === ",") {
        e.preventDefault();
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
    return <Welcome onComplete={refresh} />;
  }

  return (
    <>
      {isDesktop && <MissingCLIBanner />}
      <Switch>
        <Route path="/runs/new" component={LaunchView} />
        <Route path="/runs/:id" component={RunView} />
        <Route path="/runs" component={RunListView} />
        <Route path="/account" component={SettingsPage} />
        <Route path="/teams/:id" component={TeamPage} />
        <Route component={EditorViewWithChrome} />
      </Switch>
      <ToastContainer />
      {isDesktop && (
        <>
          <Settings open={settingsOpen} onClose={() => setSettingsOpen(false)} />
          <ProjectSwitcher open={switcherOpen} onClose={() => setSwitcherOpen(false)} />
        </>
      )}
    </>
  );
}

// EditorViewWithChrome wraps the existing editor with a small header
// chip that surfaces the current user + active team and opens the
// teams / account pages. Drawn as a fixed top-right cluster so it
// doesn't disrupt the editor's own toolbars.
function EditorViewWithChrome() {
  const { user, teams, activeTeamID, activeTeam, signOut, selectTeam } = useAuth();
  const [, navigate] = useLocation();
  const [open, setOpen] = useState(false);

  return (
    <div className="relative">
      <div className="fixed top-2 right-3 z-50">
        <button
          onClick={() => setOpen((v) => !v)}
          className="bg-surface-1/95 border border-border-subtle rounded px-3 py-1 text-xs flex items-center gap-2 shadow"
        >
          <span className="font-medium">{activeTeam?.team_name ?? "No team"}</span>
          <span className="text-fg-muted">{user?.email}</span>
          <span>▾</span>
        </button>
        {open && (
          <div
            className="absolute right-0 mt-1 w-72 bg-surface-1 border border-border-subtle rounded shadow-lg p-2 text-sm"
            onMouseLeave={() => setOpen(false)}
          >
            <div className="px-2 py-1 text-xs uppercase tracking-wider text-fg-muted">
              Switch team
            </div>
            {teams.map((t) => (
              <button
                key={t.team_id}
                onClick={() => {
                  void selectTeam(t.team_id);
                  setOpen(false);
                }}
                className={`w-full text-left px-2 py-1.5 rounded hover:bg-surface-2 ${
                  t.team_id === activeTeamID ? "bg-surface-2" : ""
                }`}
              >
                <div className="font-medium">{t.team_name}</div>
                <div className="text-xs text-fg-muted">
                  {t.role}
                  {t.personal && " · personal"}
                </div>
              </button>
            ))}
            <div className="my-1 border-t border-border-subtle" />
            {activeTeam && (
              <button
                onClick={() => {
                  navigate(`/teams/${activeTeam.team_id}`);
                  setOpen(false);
                }}
                className="w-full text-left px-2 py-1.5 rounded hover:bg-surface-2"
              >
                Manage {activeTeam.team_name}
              </button>
            )}
            <button
              onClick={() => {
                navigate("/account");
                setOpen(false);
              }}
              className="w-full text-left px-2 py-1.5 rounded hover:bg-surface-2"
            >
              Account settings
            </button>
            {user?.is_super_admin && (
              <button
                onClick={() => {
                  navigate("/admin");
                  setOpen(false);
                }}
                className="w-full text-left px-2 py-1.5 rounded hover:bg-surface-2 text-fg-warn"
              >
                Platform admin
              </button>
            )}
            <button
              onClick={() => void signOut()}
              className="w-full text-left px-2 py-1.5 rounded hover:bg-surface-2 text-fg-error"
            >
              Sign out
            </button>
          </div>
        )}
      </div>
      <EditorView />
    </div>
  );
}
