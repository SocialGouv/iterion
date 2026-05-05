import { useEffect, useState } from "react";
import { Route, Switch } from "wouter";

import EditorView from "@/components/EditorView";
import LaunchView from "@/components/Runs/LaunchView";
import RunListView from "@/components/Runs/RunListView";
import RunView from "@/components/Runs/RunView";
import ToastContainer from "@/components/shared/Toast";
import MissingCLIBanner from "@/components/MissingCLIBanner";
import Welcome from "@/views/Welcome";
import Settings from "@/views/Settings";
import ProjectSwitcher from "@/views/ProjectSwitcher";
import { useDesktop } from "@/hooks/useDesktop";
import { onDesktopEvent } from "@/lib/desktopBridge";
import { DesktopEvent } from "@/lib/desktopEvents";

export default function App() {
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
        <Route component={EditorView} />
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
