import { useEffect } from "react";
import { useLocation } from "wouter";

import { useProjectInfo } from "@/hooks/useProjectInfo";
import { desktop, isDesktop } from "@/lib/desktopBridge";
import { useRunStore } from "@/store/run";
import { useTabsStore } from "@/store/tabs";

const APP = "iterion studio";

function basename(path: string): string {
  const parts = path.split(/[\\/]/);
  return parts[parts.length - 1] || path;
}

/**
 * useDocumentTitle keeps `document.title` in sync with the route, the
 * open file / current run, and the selected project. The desktop
 * (Wails) window title mirrors document.title natively, so this hook
 * drives both the browser tab and the desktop window title.
 *
 * Format: "<context> — <project> · iterion"  (or "<context> · iterion"
 * when no project is resolved, e.g. cloud mode).
 */
export function useDocumentTitle() {
  const [location] = useLocation();
  const { name: projectName } = useProjectInfo();
  const runHeader = useRunStore((s) => s.snapshot?.run);
  // Editor title is keyed off the active editor tab — not the default
  // document store, which can carry a stale currentFilePath from a
  // prior edit session and would falsely advertise "untitled.bot" when
  // the user just navigated to /editor with no tabs open. Read both
  // the file param (workspace-backed files) and the label fallback
  // (examples opened via newEditorTab(name) have no file param but a
  // meaningful label).
  const activeEditorTabId = useTabsStore((s) => s.activeEditorTabId);
  const activeEditorTabFile = useTabsStore((s) => {
    const tab = s.tabs.find((t) => t.id === s.activeEditorTabId);
    return tab?.params.file ?? null;
  });
  const activeEditorTabLabel = useTabsStore((s) => {
    const tab = s.tabs.find((t) => t.id === s.activeEditorTabId);
    return tab?.label ?? null;
  });

  useEffect(() => {
    let context = "";
    if (location.startsWith("/runs/new")) {
      context = "Launch";
    } else if (location.startsWith("/runs/") && runHeader) {
      context = runHeader.name || runHeader.workflow_name || "Run";
    } else if (location.startsWith("/runs/")) {
      context = "Run";
    } else if (location === "/runs") {
      context = "Runs";
    } else if (location === "/account") {
      context = "Account";
    } else if (location.startsWith("/teams/")) {
      context = "Team";
    } else if (location === "/editor") {
      if (!activeEditorTabId) {
        // No tab open → the picker is showing, not a document.
        context = "Editor";
      } else if (activeEditorTabFile) {
        context = basename(activeEditorTabFile);
      } else {
        // Tab without a file param: examples (label = example name) or
        // blank scratchpads (label defaults to "untitled.bot").
        context = activeEditorTabLabel || "untitled.bot";
      }
    } else {
      // Home and any unmatched route: no per-page context, just the
      // project name (or bare app name when no project is resolved).
      context = "";
    }

    // Compose: "<context> — <project> · iterion", dropping any segment
    // that's empty. context is "" on Home and unmatched routes; project
    // is null in cloud mode.
    let title: string;
    if (context && projectName) {
      title = `${context} — ${projectName} · ${APP}`;
    } else if (context) {
      title = `${context} · ${APP}`;
    } else if (projectName) {
      title = `${projectName} · ${APP}`;
    } else {
      title = APP;
    }
    document.title = title;
    // On Linux WebKit2GTK the window manager doesn't pick up
    // `document.title` automatically — Wails exposes WindowSetTitle for
    // that. Best-effort; silently ignored in browser mode.
    if (isDesktop()) {
      desktop.setWindowTitle(title).catch(() => {
        /* binding may not be ready yet — re-runs on next deps change */
      });
    }
  }, [
    location,
    projectName,
    activeEditorTabId,
    activeEditorTabFile,
    activeEditorTabLabel,
    runHeader,
  ]);
}
