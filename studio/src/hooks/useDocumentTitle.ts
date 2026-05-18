import { useEffect } from "react";
import { useLocation } from "wouter";

import { useProjectInfo } from "@/hooks/useProjectInfo";
import { desktop, isDesktop } from "@/lib/desktopBridge";
import { useDocumentStore } from "@/store/document";
import { useRunStore } from "@/store/run";

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
  const currentFilePath = useDocumentStore((s) => s.currentFilePath);
  const runHeader = useRunStore((s) => s.snapshot?.run);

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
      // Editor route: show the open file name, or "untitled.bot" only
      // when a fresh document is being authored.
      context = currentFilePath ? basename(currentFilePath) : "untitled.bot";
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
  }, [location, projectName, currentFilePath, runHeader]);
}
