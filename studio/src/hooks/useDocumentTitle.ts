import { useEffect } from "react";
import { useLocation } from "wouter";

import { useProjectInfo } from "@/hooks/useProjectInfo";
import { desktop, isDesktop } from "@/lib/desktopBridge";
import { useRunStore } from "@/store/run";
import { UNTITLED_TAB_LABEL, useTabsStore } from "@/store/tabs";
import { useBotsStore } from "@/store/bots";
import { botDisplayLabel } from "@/lib/botLabel";

const APP = "iterion studio";

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
  // The catalog carries each bot's persona display_name; load it lazily
  // while on the editor so titles show the persona ("Featurly") instead of
  // a path/technical id.
  const bots = useBotsStore((s) => s.bots);
  const fetchBots = useBotsStore((s) => s.fetch);
  useEffect(() => {
    if (location === "/editor" && bots === null) {
      void fetchBots();
    }
  }, [location, bots, fetchBots]);

  useEffect(() => {
    // Every branch below (including the final else) assigns context, so no
    // initializer is needed — TS definite-assignment covers the use.
    let context: string;
    if (location.startsWith("/runs/new")) {
      context = "Launch";
    } else if (location.startsWith("/runs/") && runHeader) {
      context = runHeader.name || runHeader.workflow_name || "Run";
    } else if (location.startsWith("/runs/")) {
      context = "Run";
    } else if (location === "/runs") {
      context = "Runs";
    } else if (location.startsWith("/pipelines/cards/")) {
      context = "Pipeline card";
    } else if (location === "/pipelines" || location.startsWith("/pipelines/")) {
      context = "Pipelines";
    } else if (location === "/account") {
      context = "Account";
    } else if (location.startsWith("/teams/")) {
      context = "Team";
    } else if (location === "/editor") {
      if (!activeEditorTabId) {
        // No tab open → the picker is showing, not a document.
        context = "Editor";
      } else if (activeEditorTabFile) {
        // Persona display_name / technical id for a bot bundle's main.bot;
        // meaningful basename for loose files.
        context = botDisplayLabel(activeEditorTabFile, bots);
      } else {
        // Tab with only a label (example opened via newEditorTab): the
        // label is already the display name, kept current by
        // EditorTabHost's TabLabelSync.
        context = activeEditorTabLabel || UNTITLED_TAB_LABEL;
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
    bots,
    runHeader,
  ]);
}
