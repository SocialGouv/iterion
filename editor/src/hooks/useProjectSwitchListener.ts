import { useEffect } from "react";
import { useLocation } from "wouter";

import { fileWatcher } from "@/api/ws";
import { refreshServerProjects } from "@/hooks/useProjects";
import { useRunStore } from "@/store/run";
import { useServerInfoStore } from "@/store/serverInfo";
import { useUIStore } from "@/store/ui";

// useProjectSwitchListener subscribes to the global `project_switched`
// WebSocket event (pkg/server/projects.go:broadcastProjectSwitched) and
// resets the SPA to a clean state on receipt:
//
//   1. Clears the run-store (running/finished runs of the old project
//      would otherwise still be visible on the new project's home).
//   2. Refetches /api/server/info so ProjectLabel + run-list scope
//      pick up the new work_dir.
//   3. Refreshes the projects MRU so the highlighted "current" row
//      tracks the new selection.
//   4. Navigates to "/" — the new project's home — so the user lands
//      on a familiar surface instead of a 404 from a run id that
//      belongs to the previous store.
//   5. Surfaces a toast so the swap is visible even if the user was
//      reading logs and missed the navigation.
//
// Mount once in AuthedApp so the listener is global to the session.
export function useProjectSwitchListener(): void {
  const [, setLocation] = useLocation();
  useEffect(() => {
    fileWatcher.connect();
    const off = fileWatcher.subscribe((event) => {
      if (event.type !== "project_switched") return;
      useRunStore.getState().reset();
      void useServerInfoStore.getState().refresh();
      refreshServerProjects();
      setLocation("/");
      useUIStore.getState().addToast(
        `Switched to ${event.current.name}`,
        "info",
      );
    });
    return () => {
      off();
    };
  }, [setLocation]);
}
