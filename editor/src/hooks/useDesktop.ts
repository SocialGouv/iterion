import { useCallback, useEffect, useState } from "react";

import { desktop, isDesktop, onDesktopEvent, type Project } from "@/lib/desktopBridge";
import { DesktopEvent } from "@/lib/desktopEvents";

interface UseDesktopState {
  isDesktop: boolean;
  ready: boolean;
  firstRunPending: boolean;
  projects: Project[];
  currentProject: Project | null;
}

interface UseDesktopAPI extends UseDesktopState {
  refresh: () => Promise<void>;
  switchProject: (id: string) => Promise<void>;
  addProject: (dir: string) => Promise<Project>;
  removeProject: (id: string) => Promise<void>;
  pickAndAddProject: () => Promise<Project | null>;
}

const initial: UseDesktopState = {
  isDesktop: false,
  ready: false,
  firstRunPending: false,
  projects: [],
  currentProject: null,
};

/**
 * useDesktop centralises desktop-mode state for the editor SPA. In browser
 * mode it returns `isDesktop=false, ready=true` immediately and every
 * action throws "Not available in browser mode".
 *
 * Re-bootstrap on server restart is driven by the Go side: when the desktop
 * App restarts the embedded HTTP server (SwitchProject, AddProjectSilently),
 * it calls Wails' WindowReloadApp, which navigates the webview back to the
 * AssetServer's start URL (wails:// on Mac/Linux, http://wails.localhost on
 * Windows). The AssetServer's reverse-proxy handler (cmd/iterion-desktop/
 * asset_proxy.go) routes that fresh load to the NEW embedded server (its
 * cache rebuilds when serverURL changes), so the SPA re-mounts on a
 * working backend with /wails/runtime.js + /wails/ipc.js still injected
 * (because the page origin remains the AssetServer's). The session-token
 * cookie is attached server-side by the proxy on every forwarded request,
 * so the SPA never has to learn or re-issue it on switch.
 *
 * We deliberately do NOT call window.location.reload() on project:switched
 * here: with the proxy architecture, the page URL is still the AssetServer
 * URL — but any local state (in-memory WS subscriptions, react query
 * caches) needs to be torn down on a real switch. WindowReloadApp does
 * exactly that. The project:switched event is therefore a soft signal —
 * we refresh React state for any consumers that mounted before reload
 * kicks in.
 */
export function useDesktop(): UseDesktopAPI {
  const [state, setState] = useState<UseDesktopState>({ ...initial, isDesktop: isDesktop() });

  const refresh = useCallback(async () => {
    if (!isDesktop()) {
      setState((s) => ({ ...s, ready: true }));
      return;
    }
    try {
      const [projects, currentProject, firstRunPending] = await Promise.all([
        desktop.listProjects(),
        desktop.getCurrentProject(),
        desktop.isFirstRunPending(),
      ]);
      setState({
        isDesktop: true,
        ready: true,
        firstRunPending,
        projects: projects ?? [],
        currentProject: currentProject ?? null,
      });
    } catch (err) {
      // Bubble through with ready=true so the UI can surface an error
      // instead of perpetually showing the loading spinner.
      console.error("useDesktop: refresh failed", err);
      setState((s) => ({ ...s, ready: true }));
    }
  }, []);

  useEffect(() => {
    refresh();
    // Soft refresh on project:switched (no window.location.reload here —
    // the Go-side wruntime.WindowReloadApp drives the actual re-bootstrap
    // to the new server URL on a fresh origin/cookie). See the file-level
    // comment above for why a JS-side reload would land on a dead port.
    const off = onDesktopEvent(DesktopEvent.ProjectSwitched, () => {
      void refresh();
    });
    return () => {
      off();
    };
  }, [refresh]);

  const switchProject = useCallback(async (id: string) => {
    await desktop.switchProject(id);
    await refresh();
  }, [refresh]);

  const addProject = useCallback(async (dir: string) => {
    const p = await desktop.addProject(dir);
    await refresh();
    return p;
  }, [refresh]);

  const removeProject = useCallback(async (id: string) => {
    await desktop.removeProject(id);
    await refresh();
  }, [refresh]);

  const pickAndAddProject = useCallback(async () => {
    const dir = await desktop.pickProjectDirectory();
    if (!dir) return null;
    return addProject(dir);
  }, [addProject]);

  return {
    ...state,
    refresh,
    switchProject,
    addProject,
    removeProject,
    pickAndAddProject,
  };
}
