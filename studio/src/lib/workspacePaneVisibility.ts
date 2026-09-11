import { useEffect, useState } from "react";

import { scopePrefix } from "./scope";

export const WORKSPACE_PANE_VISIBILITY = "workspace-pane-visibility";
export const WORKSPACE_PANE_VISIBILITY_REQUEST =
  "workspace-pane-visibility-request";
export const WORKSPACE_UNREAD_COUNT = "workspace-unread-count";

function scopedProjectId(): string | null {
  const scope = scopePrefix();
  return scope ? scope.slice(3) : null;
}

export function workspacePaneStartsVisible(
  projectId: string | null,
  currentWindow: Pick<Window, "parent">,
): boolean {
  return projectId === null || currentWindow.parent === currentWindow;
}

export function isWorkspacePaneVisibilityMessage(
  event: MessageEvent,
  parent: Window,
  origin: string,
  projectId: string,
): event is MessageEvent<{
  source: "iterion-shell";
  type: typeof WORKSPACE_PANE_VISIBILITY;
  projectId: string;
  visible: boolean;
}> {
  const data = event.data as Record<string, unknown> | null;
  return (
    event.origin === origin &&
    event.source === parent &&
    data?.source === "iterion-shell" &&
    data.type === WORKSPACE_PANE_VISIBILITY &&
    data.projectId === projectId &&
    typeof data.visible === "boolean"
  );
}

/** The shell is authoritative because hidden same-origin iframes stay alive. */
export function useWorkspacePaneVisibility(): boolean {
  const projectId = scopedProjectId();
  const [visible, setVisible] = useState(() =>
    typeof window === "undefined"
      ? true
      : workspacePaneStartsVisible(projectId, window),
  );

  useEffect(() => {
    if (!projectId || window.parent === window) {
      return;
    }
    const onMessage = (event: MessageEvent) => {
      if (
        isWorkspacePaneVisibilityMessage(
          event,
          window.parent,
          window.location.origin,
          projectId,
        )
      ) {
        setVisible(event.data.visible);
      }
    };
    window.addEventListener("message", onMessage);
    window.parent.postMessage(
      {
        source: "iterion-pane",
        type: WORKSPACE_PANE_VISIBILITY_REQUEST,
        projectId,
      },
      window.location.origin,
    );
    return () => window.removeEventListener("message", onMessage);
  }, [projectId]);

  return visible;
}

export function postWorkspaceUnreadCount(count: number): void {
  const projectId = scopedProjectId();
  if (!projectId || typeof window === "undefined" || window.parent === window) {
    return;
  }
  window.parent.postMessage(
    {
      source: "iterion-pane",
      type: WORKSPACE_UNREAD_COUNT,
      projectId,
      count: Math.max(0, Math.min(8, Math.trunc(count))),
    },
    window.location.origin,
  );
}
