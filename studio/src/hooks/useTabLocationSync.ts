import { useEffect, useRef } from "react";
import { useLocation } from "wouter";

import {
  HOME_TAB_ID,
  useTabsStore,
  type Tab,
  type TabKind,
} from "@/store/tabs";

// useTabLocationSync keeps the active tab and the wouter URL in lock-step
// during Phase 1, where wouter's <Switch> is still the real renderer and
// tabs are just a stateful navigation skin on top.
//
// Sync goes both ways:
//   - active tab changes → push the matching URL via setLocation
//   - URL changes (deep link, browser back) → openTab to focus/create
//
// The loop closes because both setActive() and openTab() are no-ops when
// the requested state is already current, so neither side fires a second
// update.

interface TabIntent {
  kind: TabKind;
  params: Record<string, string>;
}

const TAB_FROM_PATH: Array<{
  test: (path: string) => boolean;
  build: (path: string, search: string) => TabIntent | null;
}> = [
  {
    test: (p) => p === "" || p === "/",
    build: () => ({ kind: "home", params: {} }),
  },
  {
    test: (p) => p.startsWith("/editor"),
    build: (_p, search) => {
      const sp = new URLSearchParams(search);
      const file = sp.get("file");
      const params: Record<string, string> = file ? { file } : {};
      return { kind: "editor", params };
    },
  },
  {
    // /runs/new (LaunchView) intentionally returns null so it stays
    // rendered through wouter without spawning a tab.
    test: (p) => p === "/runs/new",
    build: () => null,
  },
  {
    test: (p) => p.startsWith("/runs/"),
    build: (p) => {
      const runId = p.split("/")[2] ?? "";
      if (!runId) return null;
      return { kind: "run", params: { runId } };
    },
  },
  {
    test: (p) => p === "/runs",
    build: () => null,
  },
  {
    test: (p) => p.startsWith("/whats-next"),
    build: () => ({ kind: "whats-next", params: {} }),
  },
  {
    test: (p) => p.startsWith("/board"),
    build: () => ({ kind: "board", params: {} }),
  },
  {
    test: (p) => p.startsWith("/dispatcher"),
    build: () => ({ kind: "dispatcher", params: {} }),
  },
  {
    test: (p) => p.startsWith("/account"),
    build: () => ({ kind: "settings", params: {} }),
  },
  {
    test: (p) => p.startsWith("/teams/"),
    build: (p) => {
      const teamId = p.split("/")[2] ?? "";
      if (!teamId) return null;
      return { kind: "team", params: { teamId } };
    },
  },
];

function intentFromLocation(location: string): TabIntent | null {
  const [path = "", search = ""] = location.split("?");
  for (const matcher of TAB_FROM_PATH) {
    if (matcher.test(path)) return matcher.build(path, search);
  }
  return null;
}

function urlForTab(tab: Tab): string {
  switch (tab.kind) {
    case "home":
      return "/";
    case "editor":
      return tab.params.file
        ? `/editor?file=${encodeURIComponent(tab.params.file)}`
        : "/editor";
    case "run":
      return tab.params.runId
        ? `/runs/${encodeURIComponent(tab.params.runId)}`
        : "/runs";
    case "whats-next":
      return "/whats-next";
    case "board":
      return "/board";
    case "dispatcher":
      return "/dispatcher";
    case "settings":
      return "/account";
    case "team":
      return tab.params.teamId
        ? `/teams/${encodeURIComponent(tab.params.teamId)}`
        : "/";
  }
}

function matches(tab: Tab | null, intent: TabIntent | null): boolean {
  if (!tab || !intent) return false;
  if (tab.kind !== intent.kind) return false;
  const a = tab.params;
  const b = intent.params;
  const aKeys = Object.keys(a);
  if (aKeys.length !== Object.keys(b).length) return false;
  for (const k of aKeys) {
    if (a[k] !== b[k]) return false;
  }
  return true;
}

export function useTabLocationSync(): void {
  const [location, setLocation] = useLocation();
  const tabs = useTabsStore((s) => s.tabs);
  const activeTabId = useTabsStore((s) => s.activeTabId);
  const openTab = useTabsStore((s) => s.openTab);

  const activeTab = activeTabId
    ? tabs.find((t) => t.id === activeTabId) ?? null
    : null;

  // Track the last URL we pushed so we don't fight ourselves: when
  // setLocation triggers a re-render, the URL-watching effect would
  // otherwise see "new" location and re-call openTab. Comparing against
  // this ref short-circuits that.
  const lastPushedUrl = useRef<string | null>(null);

  // tab → URL
  useEffect(() => {
    if (!activeTab) return;
    const target = urlForTab(activeTab);
    if (target === location) return;
    lastPushedUrl.current = target;
    setLocation(target);
  }, [activeTab, location, setLocation]);

  // URL → tab
  useEffect(() => {
    if (location === lastPushedUrl.current) return;
    const intent = intentFromLocation(location);
    if (!intent) return;
    if (matches(activeTab, intent)) return;
    // /runs/:id and /editor?file= deep links: openTab will create or
    // focus the matching tab. Home stays special-cased so we don't
    // create duplicates if some path normalisation kicks in.
    if (intent.kind === "home") {
      const home = tabs.find((t) => t.id === HOME_TAB_ID);
      if (home) {
        useTabsStore.getState().setActive(home.id);
      }
      return;
    }
    openTab(intent.kind, intent.params);
  }, [location, activeTab, tabs, openTab]);
}
