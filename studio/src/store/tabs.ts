import { create } from "zustand";
import { persist, createJSONStorage } from "zustand/middleware";

import { disposeRunStore } from "@/store/run";

export type TabKind =
  | "home"
  | "editor"
  | "run"
  | "whats-next"
  | "board"
  | "dispatcher"
  | "settings"
  | "team";

export interface Tab {
  id: string;
  kind: TabKind;
  params: Record<string, string>;
  label: string;
  hydrated: boolean;
}

export const HOME_TAB_ID = "home";

// SINGLE_INSTANCE_KINDS — only one tab per kind is allowed. Opening a
// second one focuses the existing instance. The "editor" and "run"
// kinds are intentionally absent: they can be multiplied to host
// several files / runs in parallel.
const SINGLE_INSTANCE_KINDS = new Set<TabKind>([
  "home",
  "whats-next",
  "board",
  "dispatcher",
  "settings",
]);

interface TabsState {
  tabs: Tab[];
  activeTabId: string | null;
  openTab: (kind: TabKind, params?: Record<string, string>, label?: string) => string;
  closeTab: (id: string) => void;
  setActive: (id: string) => void;
  reorder: (from: number, to: number) => void;
  rename: (id: string, label: string) => void;
}

function generateId(): string {
  if (typeof crypto !== "undefined" && "randomUUID" in crypto) {
    return crypto.randomUUID();
  }
  return `t_${Date.now().toString(36)}_${Math.random().toString(36).slice(2, 8)}`;
}

function defaultLabelFor(kind: TabKind, params: Record<string, string>): string {
  switch (kind) {
    case "home":
      return "Home";
    case "editor":
      return params.file ? (params.file.split(/[/\\]/).pop() ?? "Editor") : "Editor";
    case "run":
      return params.runId ? params.runId.slice(0, 8) : "Run";
    case "whats-next":
      return "What's Next";
    case "board":
      return "Board";
    case "dispatcher":
      return "Dispatcher";
    case "settings":
      return "Settings";
    case "team":
      return params.teamId ? `Team ${params.teamId.slice(0, 6)}` : "Team";
  }
}

function paramsEqual(a: Record<string, string>, b: Record<string, string>): boolean {
  const aKeys = Object.keys(a);
  const bKeys = Object.keys(b);
  if (aKeys.length !== bKeys.length) return false;
  for (const k of aKeys) {
    if (a[k] !== b[k]) return false;
  }
  return true;
}

function findExistingTab(
  tabs: Tab[],
  kind: TabKind,
  params: Record<string, string>,
): Tab | undefined {
  if (SINGLE_INSTANCE_KINDS.has(kind)) {
    return tabs.find((t) => t.kind === kind);
  }
  return tabs.find((t) => t.kind === kind && paramsEqual(t.params, params));
}

function homeTab(): Tab {
  return {
    id: HOME_TAB_ID,
    kind: "home",
    params: {},
    label: "Home",
    hydrated: true,
  };
}

export const useTabsStore = create<TabsState>()(
  persist(
    (set, get) => ({
      tabs: [homeTab()],
      activeTabId: HOME_TAB_ID,
      openTab: (kind, params = {}, label) => {
        const existing = findExistingTab(get().tabs, kind, params);
        if (existing) {
          set((s) =>
            s.activeTabId === existing.id
              ? s
              : {
                  activeTabId: existing.id,
                  tabs: existing.hydrated
                    ? s.tabs
                    : s.tabs.map((t) =>
                        t.id === existing.id ? { ...t, hydrated: true } : t,
                      ),
                },
          );
          return existing.id;
        }
        const id = generateId();
        const tab: Tab = {
          id,
          kind,
          params,
          label: label ?? defaultLabelFor(kind, params),
          // Freshly opened tabs are hydrated by definition (the user
          // just asked to navigate to them). Only tabs restored from
          // localStorage start dormant — handled in onRehydrateStorage.
          hydrated: true,
        };
        set((s) => ({ tabs: [...s.tabs, tab], activeTabId: id }));
        return id;
      },
      closeTab: (id) => {
        if (id === HOME_TAB_ID) return;
        set((s) => {
          const idx = s.tabs.findIndex((t) => t.id === id);
          if (idx === -1) return s;
          const closed = s.tabs[idx];
          const tabs = s.tabs.filter((t) => t.id !== id);
          // Dispose the per-runId Zustand store + WS connection now
          // that no tab references this run. Done inside the set()
          // callback so we hold the same authoritative tabs list when
          // checking for other tabs that might still reference it.
          if (closed?.kind === "run" && closed.params.runId) {
            const stillReferenced = tabs.some(
              (t) => t.kind === "run" && t.params.runId === closed.params.runId,
            );
            if (!stillReferenced) {
              disposeRunStore(closed.params.runId);
            }
          }
          let activeTabId = s.activeTabId;
          if (activeTabId === id) {
            activeTabId = tabs[idx - 1]?.id ?? tabs[idx]?.id ?? HOME_TAB_ID;
          }
          return { tabs, activeTabId };
        });
      },
      setActive: (id) => {
        set((s) => {
          if (s.activeTabId === id) return s;
          const target = s.tabs.find((t) => t.id === id);
          if (!target) return s;
          const tabs = target.hydrated
            ? s.tabs
            : s.tabs.map((t) => (t.id === id ? { ...t, hydrated: true } : t));
          return { tabs, activeTabId: id };
        });
      },
      reorder: (from, to) => {
        set((s) => {
          if (from === to) return s;
          if (from < 0 || from >= s.tabs.length) return s;
          if (to < 0 || to >= s.tabs.length) return s;
          const tabs = s.tabs.slice();
          const [moved] = tabs.splice(from, 1);
          if (!moved) return s;
          tabs.splice(to, 0, moved);
          return { tabs };
        });
      },
      rename: (id, label) => {
        set((s) => ({
          tabs: s.tabs.map((t) => (t.id === id ? { ...t, label } : t)),
        }));
      },
    }),
    {
      name: "iterion.tabs",
      storage: createJSONStorage(() => localStorage),
      // Drop transient runtime fields from persistence — `hydrated` is
      // intentionally rebuilt on reload (everything starts dormant
      // except Home), and method properties are added by the create()
      // initializer anyway.
      partialize: (state) => ({
        tabs: state.tabs.map((t) => ({
          id: t.id,
          kind: t.kind,
          params: t.params,
          label: t.label,
          hydrated: false,
        })),
        activeTabId: state.activeTabId,
      }),
      onRehydrateStorage: () => (state) => {
        if (!state) return;
        // Guarantee a Home tab so the app always has a fallback.
        if (!state.tabs.some((t) => t.id === HOME_TAB_ID)) {
          state.tabs = [homeTab(), ...state.tabs];
        }
        // Home is always hydrated; others stay dormant until activated.
        state.tabs = state.tabs.map((t) =>
          t.id === HOME_TAB_ID ? { ...t, hydrated: true } : t,
        );
        // The active tab on reload must be Home until the user clicks
        // elsewhere — that way the cold start doesn't fetch anything.
        // (Without this, restoring `activeTabId` to e.g. a run tab
        // would force immediate WS + snapshot fetch on every reload,
        // defeating "lazy restoration".)
        state.activeTabId = HOME_TAB_ID;
      },
    },
  ),
);

export function useActiveTab(): Tab | null {
  return useTabsStore((s) => {
    const id = s.activeTabId;
    if (!id) return null;
    return s.tabs.find((t) => t.id === id) ?? null;
  });
}
