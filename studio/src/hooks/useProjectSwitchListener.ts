import { useEffect } from "react";
import { useLocation } from "wouter";

import { fileWatcher } from "@/api/ws";
import { refreshServerProjects } from "@/hooks/useProjects";
import { useBotsStore } from "@/store/bots";
import { resetAllRunStores } from "@/store/run";
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
//   3. Refetches the bot catalog (workspace-scoped on the server) so
//      the board pickers offer the new project's bots.
//   4. Refreshes the projects MRU so the highlighted "current" row
//      tracks the new selection.
//   5. Navigates to "/" — the new project's home — so the user lands
//      on a familiar surface instead of a 404 from a run id that
//      belongs to the previous store.
//   6. Surfaces a toast so the swap is visible even if the user was
//      reading logs and missed the navigation.
//
// Mount once in AuthedApp so the listener is global to the session.
export function useProjectSwitchListener(): void {
  const [, setLocation] = useLocation();
  useEffect(() => {
    fileWatcher.connect();
    const off = fileWatcher.subscribe((event) => {
      if (event.type !== "project_switched") return;
      resetAllRunStores();
      void useServerInfoStore.getState().refresh();
      // The bot catalog is workspace-scoped (server derives it from the
      // current work_dir): refetch so the board pickers / AddTaskDialog
      // offer the NEW project's bots instead of the cached previous list.
      void useBotsStore.getState().refetch();
      refreshServerProjects();
      setLocation("/");
      useUIStore.getState().addToast(
        `Switched to ${event.current.name}`,
        "info",
      );
    });
    return () => {
      off();
      // Release our refCount on the shared file-watcher socket. Without this,
      // every mount (incl. StrictMode double-invoke and any AuthedApp remount)
      // leaks a +1 and the singleton WS never tears down.
      fileWatcher.disconnect();
    };
  }, [setLocation]);
}
