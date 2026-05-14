import { useEffect, useRef } from "react";
import { create } from "zustand";

import {
  desktop,
  isDesktop,
  onDesktopEvent,
  type Project,
} from "@/lib/desktopBridge";
import { DesktopEvent } from "@/lib/desktopEvents";
import { useUIStore } from "@/store/ui";

interface DesktopState {
  isDesktop: boolean;
  ready: boolean;
  firstRunPending: boolean;
  projects: Project[];
  currentProject: Project | null;
  // Number of components subscribed to desktop state. The shared
  // listeners (projects:changed, project:switched, initial fetch) run
  // exactly once: the first useDesktop mount installs them, the last
  // unmount tears them down. Without this guard, navigating between
  // views would either leak listeners or refetch on every mount.
  _refCount: number;
  _cleanup: (() => void) | null;
}

interface DesktopActions {
  refresh: () => Promise<void>;
  acquire: () => void;
  release: () => void;
}

const initial: Omit<DesktopState, "_refCount" | "_cleanup"> = {
  isDesktop: false,
  ready: false,
  firstRunPending: false,
  projects: [],
  currentProject: null,
};

// Singleton store — every useDesktop() call shares the same state
// object. Previously useDesktop used per-component useState, which
// meant a refresh triggered inside (say) the ProjectSwitcher would
// only update *its* state — the toolbar's project chip kept showing
// the stale list. Centralising here also makes the desktop event
// listeners ref-counted instead of duplicated per-mount.
const useStore = create<DesktopState & DesktopActions>((set, get) => ({
  ...initial,
  isDesktop: isDesktop(),
  _refCount: 0,
  _cleanup: null,

  refresh: async () => {
    if (!isDesktop()) {
      set({ ready: true });
      return;
    }
    try {
      const [projects, currentProject, firstRunPending] = await Promise.all([
        desktop.listProjects(),
        desktop.getCurrentProject(),
        desktop.isFirstRunPending(),
      ]);
      set({
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
      set({ ready: true });
    }
  },

  acquire: () => {
    const state = get();
    const next = state._refCount + 1;
    if (next === 1) {
      // First subscriber — kick off the initial fetch and wire desktop
      // event listeners. Both project:switched (current pointer flip)
      // and projects:changed (list mutated) drive a soft refresh —
      // the latter catches non-current deletions which the former
      // doesn't fire for. See cmd/iterion-desktop/bindings.go for the
      // emit sites.
      void state.refresh();
      const offs = [
        onDesktopEvent(DesktopEvent.ProjectSwitched, () => {
          void get().refresh();
        }),
        onDesktopEvent(DesktopEvent.ProjectsChanged, () => {
          void get().refresh();
        }),
      ];
      set({
        _refCount: next,
        _cleanup: () => offs.forEach((off) => off()),
      });
      return;
    }
    set({ _refCount: next });
  },

  release: () => {
    const state = get();
    const next = Math.max(0, state._refCount - 1);
    if (next === 0 && state._cleanup) {
      state._cleanup();
      set({ _refCount: next, _cleanup: null });
      return;
    }
    set({ _refCount: next });
  },
}));

interface UseDesktopAPI {
  isDesktop: boolean;
  ready: boolean;
  firstRunPending: boolean;
  projects: Project[];
  currentProject: Project | null;
  refresh: () => Promise<void>;
  switchProject: (id: string) => Promise<void>;
  addProject: (dir: string) => Promise<Project>;
  removeProject: (id: string) => Promise<void>;
  pickAndAddProject: () => Promise<Project | null>;
}

/**
 * useDesktop exposes the shared desktop-mode state for the editor SPA.
 * In browser mode it returns `isDesktop=false, ready=true` immediately
 * and every action throws "Not available in browser mode".
 *
 * Re-bootstrap on server restart is driven by the Go side: when the
 * desktop App restarts the embedded HTTP server (SwitchProject,
 * AddProjectSilently), it calls Wails' WindowReloadApp which navigates
 * the webview back to the AssetServer's start URL. The
 * project:switched event is a soft signal so React state stays fresh
 * for any consumers that mounted before reload kicks in.
 *
 * State is held in a Zustand singleton so every consumer sees the
 * same projects/currentProject. The store ref-counts subscribers,
 * fetching once on the first mount and tearing down listeners on the
 * last unmount.
 */
export function useDesktop(): UseDesktopAPI {
  const isDesktopVal = useStore((s) => s.isDesktop);
  const ready = useStore((s) => s.ready);
  const firstRunPending = useStore((s) => s.firstRunPending);
  const projects = useStore((s) => s.projects);
  const currentProject = useStore((s) => s.currentProject);
  const refresh = useStore((s) => s.refresh);
  const acquire = useStore((s) => s.acquire);
  const release = useStore((s) => s.release);
  // Ref-count once per mount via a guard so React 18 strict-mode's
  // double-invocation doesn't double-acquire (it cleans up between
  // the two mounts, so the second acquire still nets out correctly,
  // but the ref guard avoids the transient teardown).
  const acquired = useRef(false);
  useEffect(() => {
    if (!acquired.current) {
      acquired.current = true;
      acquire();
    }
    return () => {
      acquired.current = false;
      release();
    };
  }, [acquire, release]);

  return {
    isDesktop: isDesktopVal,
    ready,
    firstRunPending,
    projects,
    currentProject,
    refresh,
    switchProject: async (id: string) => {
      try {
        await desktop.switchProject(id);
      } catch (err) {
        notifyDesktopError("Switch project failed", err);
        throw err;
      }
      await refresh();
    },
    addProject: async (dir: string) => {
      let p: Project;
      try {
        p = await desktop.addProject(dir);
      } catch (err) {
        notifyDesktopError("Add project failed", err);
        throw err;
      }
      await refresh();
      return p;
    },
    removeProject: async (id: string) => {
      try {
        await desktop.removeProject(id);
      } catch (err) {
        notifyDesktopError("Remove project failed", err);
        throw err;
      }
      await refresh();
    },
    pickAndAddProject: async () => {
      const dir = await desktop.pickProjectDirectory();
      if (!dir) return null;
      let p: Project;
      try {
        p = await desktop.addProject(dir);
      } catch (err) {
        notifyDesktopError("Add project failed", err);
        throw err;
      }
      await refresh();
      return p;
    },
  };
}

// Surface backend errors via the global toast system. Previously
// useDesktop swallowed them silently (only logged on refresh) — so a
// failed removeProject looked like a no-op to the user.
function notifyDesktopError(label: string, err: unknown): void {
  const msg = err instanceof Error ? err.message : String(err);
  console.error(`useDesktop: ${label}`, err);
  try {
    useUIStore.getState().addToast(`${label}: ${msg}`, "error");
  } catch {
    // ui store may not be initialised in tests
  }
}
