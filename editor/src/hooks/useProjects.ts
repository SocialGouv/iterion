import { useCallback, useEffect, useRef } from "react";
import { create } from "zustand";

import * as projectsApi from "@/api/projects";
import type { Project } from "@/api/projects";
import { useDesktop } from "@/hooks/useDesktop";
import { isDesktop } from "@/lib/desktopBridge";
import { useServerInfoStore } from "@/store/serverInfo";

function findCurrent(
  list: Project[],
  currentID: string | undefined | null,
): Project | null {
  if (!currentID) return null;
  return list.find((p) => p.id === currentID) ?? null;
}

// useProjects exposes a single API shape over both project sources:
//   - Desktop (Wails): delegates to useDesktop, which talks to the
//     embedded Go bindings (ListProjects/SwitchProject/...).
//   - Browser/server: hits the HTTP endpoints in pkg/server/projects.go.
// Cloud mode is signalled via `enabled=false` for now — a follow-up
// will add Mongo-backed tenant workspaces sharing the same hook API.

export interface ProjectsAPI {
  enabled: boolean;
  ready: boolean;
  projects: Project[];
  currentProject: Project | null;
  refresh: () => Promise<void>;
  switchProject: (id: string) => Promise<void>;
  addProject: (dir: string) => Promise<void>;
  removeProject: (id: string) => Promise<void>;
}

interface ServerProjectsState {
  ready: boolean;
  projects: Project[];
  currentProject: Project | null;
  // refCount + cleanup mirror the pattern in useDesktop: a singleton
  // store so every consumer sees the same list, refetched once on
  // first mount.
  _refCount: number;
  refresh: () => Promise<void>;
  acquire: () => void;
  release: () => void;
}

const useServerProjectsStore = create<ServerProjectsState>((set, get) => ({
  ready: false,
  projects: [],
  currentProject: null,
  _refCount: 0,

  refresh: async () => {
    try {
      const projects = (await projectsApi.listProjects()) ?? [];
      // current_project_id is part of /api/server/info, so we derive
      // the current project from the list rather than firing a second
      // HTTP round-trip + disk read on the server.
      const info = useServerInfoStore.getState().info;
      const currentProject = findCurrent(projects, info?.current_project_id);
      set({ ready: true, projects, currentProject });
    } catch (err) {
      console.error("useProjects: refresh failed", err);
      set({ ready: true });
    }
  },

  acquire: () => {
    const next = get()._refCount + 1;
    if (next === 1) {
      void get().refresh();
    }
    set({ _refCount: next });
  },

  release: () => {
    const next = Math.max(0, get()._refCount - 1);
    set({ _refCount: next });
  },
}));

// notifyServerProjectsChanged is wired into the global WS listener
// (useProjectSwitchListener) so the singleton refetches whenever a
// project_switched event lands. Exposed here so the listener doesn't
// need a hook context.
export function refreshServerProjects(): void {
  void useServerProjectsStore.getState().refresh();
}

function useServerProjects(): ProjectsAPI {
  const ready = useServerProjectsStore((s) => s.ready);
  const projects = useServerProjectsStore((s) => s.projects);
  const currentProject = useServerProjectsStore((s) => s.currentProject);
  const refresh = useServerProjectsStore((s) => s.refresh);
  const acquire = useServerProjectsStore((s) => s.acquire);
  const release = useServerProjectsStore((s) => s.release);
  const serverInfo = useServerInfoStore((s) => s.info);

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

  const switchProject = useCallback(
    async (id: string) => {
      await projectsApi.switchProject(id);
      // The WS broadcast triggers refresh asynchronously; we also
      // refresh inline so the immediate post-action render is fresh
      // even if the WS hasn't reconnected yet.
      await refresh();
    },
    [refresh],
  );

  const addProject = useCallback(
    async (dir: string) => {
      await projectsApi.addProject(dir);
      await refresh();
    },
    [refresh],
  );

  const removeProject = useCallback(
    async (id: string) => {
      await projectsApi.removeProject(id);
      await refresh();
    },
    [refresh],
  );

  const enabled = serverInfo?.mode !== "cloud";

  return {
    enabled,
    ready: ready && !!serverInfo,
    projects,
    currentProject,
    refresh,
    switchProject,
    addProject,
    removeProject,
  };
}

function useDesktopProjects(): ProjectsAPI {
  const d = useDesktop();
  return {
    enabled: true,
    ready: d.ready,
    projects: d.projects,
    currentProject: d.currentProject,
    refresh: d.refresh,
    // d.addProject returns the inserted Project; ProjectsAPI returns
    // void so we forward and discard. d.switchProject / d.removeProject
    // already return void.
    switchProject: d.switchProject,
    addProject: async (dir: string) => {
      await d.addProject(dir);
    },
    removeProject: d.removeProject,
  };
}

/**
 * Unified project registry hook. Branches at call time on isDesktop():
 * - desktop → Wails bindings (existing useDesktop)
 * - browser → HTTP /api/projects backed by pkg/server/projects.go
 *
 * The two implementations expose the same shape so consumers
 * (ProjectSwitcher, ProjectLabel, AddProjectDialog) don't need to
 * know which mode they're in.
 */
export function useProjects(): ProjectsAPI {
  // The desktop and server hooks have different rules-of-hooks
  // requirements (desktop's ref-counting effect can't run in browser
  // mode and vice versa). Branching at the call site is safe because
  // isDesktop() is a constant for the lifetime of the SPA.
  if (isDesktop()) {
    // eslint-disable-next-line react-hooks/rules-of-hooks
    return useDesktopProjects();
  }
  // eslint-disable-next-line react-hooks/rules-of-hooks
  return useServerProjects();
}
