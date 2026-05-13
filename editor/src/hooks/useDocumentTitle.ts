import { useEffect } from "react";
import { useLocation } from "wouter";

import { useProjectInfo } from "@/hooks/useProjectInfo";
import { useDocumentStore } from "@/store/document";
import { useRunStore } from "@/store/run";

const APP = "iterion";

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
    } else if (currentFilePath) {
      context = basename(currentFilePath);
    } else {
      context = "untitled.iter";
    }

    const parts = [context];
    if (projectName) parts.push(projectName);
    parts.push(APP);
    // First two segments are joined with em-dash for readability;
    // the trailing app name is bullet-separated.
    let title: string;
    if (parts.length === 3) {
      title = `${parts[0]} — ${parts[1]} · ${parts[2]}`;
    } else {
      title = `${parts[0]} · ${parts[1]}`;
    }
    document.title = title;
  }, [location, projectName, currentFilePath, runHeader]);
}
