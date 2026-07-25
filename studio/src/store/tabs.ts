import { create } from "zustand";
import { persist, createJSONStorage } from "zustand/middleware";

import { disposeRunStore } from "@/store/run";
import { disposeDocumentStore } from "@/store/document";
import { disposeSelectionStore } from "@/store/selection";

// disposeEditorTab releases every per-tab store an editor subtree
// holds. Centralised so adding a new per-tab store later (e.g. a
// per-tab UI slice) only requires touching one call site rather than
// remembering to extend closeTab.
function disposeEditorTab(tabId: string) {
  disposeDocumentStore(tabId);
  disposeSelectionStore(tabId);
}

// Only the views that benefit from true parallelism (multiple files
// edited side-by-side, multiple runs watched side-by-side) live in
// the tabs system. Sidebar nav covers everything else (Home, What's
// Next, Runs list, Board, Dispatcher, Settings, Team) — those screens
// open once and don't need tab semantics.
export type TabKind = "editor" | "run";

export interface Tab {
  id: string;
  kind: TabKind;
  params: Record<string, string>;
  label: string;
  // projectKey scopes a tab to one project (the project's dir, from
  // useProjectInfo). Tabs only show when their projectKey matches the
  // active project (selectRunTabs / selectEditorTabs), so switching
  // projects hides the others — preserved for switch-back — and you never
  // see another project's run/editor tab. undefined = legacy (dropped on
  // rehydrate); scoping is off when currentProjectKey is null (cloud mode,
  // no folder).
  projectKey?: string;
  // hydrated stays false on cold start for tabs restored from localStorage
  // and flips true on first activation. Lets the editor/run tab views
  // skip mounting (and thus skip api.openFile + WS) for tabs the user
  // hasn't clicked yet, even though they're shown in the inner strip.
  hydrated: boolean;
  // restored is true for tabs rehydrated from localStorage (set by
  // onRehydrateStorage, never persisted). Lets EditorTabHost tell a
  // restored named tab that lost its file binding (must show an explicit
  // error, never a fresh scaffold) apart from an in-flight open in the
  // current session (example fork, toolbar Open) that legitimately has
  // no params.file yet.
  restored?: boolean;
}

interface TabsState {
  tabs: Tab[];
  // Each kind has its own active tab. Switching from /editor to
  // /runs/:id and back must restore the editor's last-active tab,
  // independently of run-tab activity, so we track them separately.
  activeEditorTabId: string | null;
  activeRunTabId: string | null;
  // currentProjectKey is the active project's dir (synced from
  // useProjectInfo via useProjectScopeSync). Tabs are filtered to it so
  // each project shows only its own. null in cloud mode (no folder) =
  // scoping off. NOT persisted — re-synced from the server on load.
  currentProjectKey: string | null;
  // Per-runId counter bumped each time a run is OPENED via navigation
  // (board, runs list, deep-link) — NOT on a plain tab-strip click. The
  // run log view watches it to re-enforce "follow tail" on (re)open,
  // while a tab-click preserves the user's current follow state. See
  // RunsTabsView (skipReenforceRef) + LogLinesView. Ephemeral: not
  // persisted (a reload re-mounts the log view, which defaults to follow).
  runOpenNonce: Record<string, number>;
  openTab: (kind: TabKind, params: Record<string, string>, label?: string) => string;
  // newEditorTab always creates a fresh untitled tab — used by the "+"
  // button. openTab("editor", {}) by contrast focuses an existing
  // untitled tab when one exists, which is the right behavior for
  // URL-driven navigation but not for an explicit "new tab" click.
  newEditorTab: (label?: string) => string;
  closeTab: (id: string) => void;
  setActive: (id: string) => void;
  // clearActiveEditor deselects the active editor tab (→ null) without closing
  // anything, so the /editor route can show its home/welcome pane again even
  // while tabs stay open. Used by the "Editor" nav link.
  clearActiveEditor: () => void;
  // Bump runOpenNonce[runId] — called by navigation-driven opens to
  // signal the log view to re-enforce follow-tail.
  bumpRunOpen: (runId: string) => void;
  // setCurrentProjectKey updates the active project scope and repoints the
  // active run/editor tab to one visible in the new project.
  setCurrentProjectKey: (key: string | null) => void;
  reorder: (kind: TabKind, from: number, to: number) => void;
  rename: (id: string, label: string) => void;
  // bindFile back-syncs the document's file path onto an editor tab's
  // params so persistence keeps the tab ↔ file binding. Without it a tab
  // whose document was loaded outside openTab (toolbar Open, Save As,
  // example fork) restores after a reload with no file param and cannot
  // reload its document.
  bindFile: (id: string, file: string) => void;
}

function generateId(): string {
  if (typeof crypto !== "undefined" && "randomUUID" in crypto) {
    return crypto.randomUUID();
  }
  return `t_${Date.now().toString(36)}_${Math.random().toString(36).slice(2, 8)}`;
}

// UNTITLED_TAB_LABEL is the default label newEditorTab assigns to a
// fresh unbound tab. Exported (with isDefaultTabLabel) as the single
// source of truth for "this tab never named a real file" checks —
// EditorTabHost's lost-binding detection and useDocumentTitle's
// fallback both key off it.
export const UNTITLED_TAB_LABEL = "untitled.bot";

// isDefaultTabLabel reports whether a label is one a fresh unbound
// editor tab can carry (newEditorTab's default, or defaultLabelFor
// ("editor", {})). Any OTHER label on a tab without a file param names
// a real document the tab can no longer reach.
export function isDefaultTabLabel(label: string): boolean {
  return label === UNTITLED_TAB_LABEL || label === "Editor";
}

function defaultLabelFor(kind: TabKind, params: Record<string, string>): string {
  if (kind === "editor") {
    return params.file ? (params.file.split(/[/\\]/).pop() ?? "Editor") : "Editor";
  }
  // For UUIDv7 run IDs the leading chars encode the timestamp and
  // are nearly identical between runs started close in time; the
  // trailing chars are random and distinguishing. Legacy "run_<ms>"
  // IDs also have their distinctive bits at the tail.
  return params.runId ? params.runId.slice(-8) : "Run";
}

export function paramsEqual(
  a: Record<string, string>,
  b: Record<string, string>,
): boolean {
  const aKeys = Object.keys(a);
  if (aKeys.length !== Object.keys(b).length) return false;
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
  return tabs.find((t) => t.kind === kind && paramsEqual(t.params, params));
}

function activeIdField(kind: TabKind): "activeEditorTabId" | "activeRunTabId" {
  return kind === "editor" ? "activeEditorTabId" : "activeRunTabId";
}

export const useTabsStore = create<TabsState>()(
  persist(
    (set, get) => ({
      tabs: [],
      activeEditorTabId: null,
      activeRunTabId: null,
      currentProjectKey: null,
      runOpenNonce: {},
      openTab: (kind, params, label) => {
        const existing = findExistingTab(get().tabs, kind, params);
        if (existing) {
          set((s) => {
            const field = activeIdField(existing.kind);
            const changes: Partial<TabsState> = {};
            if (s[field] !== existing.id) changes[field] = existing.id;
            if (!existing.hydrated) {
              changes.tabs = s.tabs.map((t) =>
                t.id === existing.id ? { ...t, hydrated: true } : t,
              );
            }
            return Object.keys(changes).length === 0 ? s : changes;
          });
          return existing.id;
        }
        const id = generateId();
        const tab: Tab = {
          id,
          kind,
          params,
          label: label ?? defaultLabelFor(kind, params),
          projectKey: get().currentProjectKey ?? undefined,
          hydrated: true,
        };
        set((s) => ({
          tabs: [...s.tabs, tab],
          [activeIdField(kind)]: id,
        }));
        return id;
      },
      newEditorTab: (label) => {
        const id = generateId();
        const tab: Tab = {
          id,
          kind: "editor",
          params: {},
          label: label ?? UNTITLED_TAB_LABEL,
          projectKey: get().currentProjectKey ?? undefined,
          hydrated: true,
        };
        set((s) => ({ tabs: [...s.tabs, tab], activeEditorTabId: id }));
        return id;
      },
      closeTab: (id) => {
        set((s) => {
          const idx = s.tabs.findIndex((t) => t.id === id);
          if (idx === -1) return s;
          const closed = s.tabs[idx]!;
          const tabs = s.tabs.filter((t) => t.id !== id);
          if (closed.kind === "run" && closed.params.runId) {
            const stillReferenced = tabs.some(
              (t) => t.kind === "run" && t.params.runId === closed.params.runId,
            );
            if (!stillReferenced) {
              disposeRunStore(closed.params.runId);
            }
          } else if (closed.kind === "editor") {
            disposeEditorTab(closed.id);
          }
          const field = activeIdField(closed.kind);
          let activeId = s[field];
          if (activeId === id) {
            const sameKind = tabs.filter((t) => t.kind === closed.kind);
            // Prefer the tab immediately before the closed one within
            // the same kind; fall back to the first remaining tab of
            // that kind, then null.
            const closedIdxInKind = s.tabs
              .filter((t) => t.kind === closed.kind)
              .findIndex((t) => t.id === id);
            activeId = sameKind[closedIdxInKind - 1]?.id
              ?? sameKind[closedIdxInKind]?.id
              ?? sameKind[0]?.id
              ?? null;
          }
          return { tabs, [field]: activeId } as Partial<TabsState>;
        });
      },
      setActive: (id) => {
        set((s) => {
          const target = s.tabs.find((t) => t.id === id);
          if (!target) return s;
          const field = activeIdField(target.kind);
          if (s[field] === id && target.hydrated) return s;
          const tabs = target.hydrated
            ? s.tabs
            : s.tabs.map((t) => (t.id === id ? { ...t, hydrated: true } : t));
          return { tabs, [field]: id } as Partial<TabsState>;
        });
      },
      clearActiveEditor: () => {
        set((s) => (s.activeEditorTabId === null ? s : { activeEditorTabId: null }));
      },
      bumpRunOpen: (runId) => {
        if (!runId) return;
        set((s) => ({
          runOpenNonce: {
            ...s.runOpenNonce,
            [runId]: (s.runOpenNonce[runId] ?? 0) + 1,
          },
        }));
      },
      setCurrentProjectKey: (key) => {
        set((s) => {
          if (s.currentProjectKey === key) return s;
          // The previously-active tab may belong to a now-hidden project;
          // repoint each kind's active id to its first visible tab (or
          // null) so the views never render a hidden tab.
          const visible = (t: Tab) => key == null || t.projectKey === key;
          const stillVisible = (id: string | null) => {
            const t = s.tabs.find((x) => x.id === id);
            return !!t && visible(t);
          };
          const firstVisible = (kind: TabKind) =>
            s.tabs.find((t) => t.kind === kind && visible(t))?.id ?? null;
          return {
            currentProjectKey: key,
            activeEditorTabId: stillVisible(s.activeEditorTabId)
              ? s.activeEditorTabId
              : firstVisible("editor"),
            activeRunTabId: stillVisible(s.activeRunTabId)
              ? s.activeRunTabId
              : firstVisible("run"),
          };
        });
      },
      reorder: (kind, from, to) => {
        set((s) => {
          if (from === to) return s;
          const sameKind = s.tabs.filter((t) => t.kind === kind);
          if (from < 0 || from >= sameKind.length) return s;
          if (to < 0 || to >= sameKind.length) return s;
          const moved = sameKind[from]!;
          const reordered = sameKind.slice();
          reordered.splice(from, 1);
          reordered.splice(to, 0, moved);
          // Reinsert into the global tabs array preserving non-same-kind
          // relative order. Simpler: walk the global list and replace
          // each entry of `kind` with the next from `reordered`.
          let cursor = 0;
          const tabs = s.tabs.map((t) =>
            t.kind === kind ? reordered[cursor++]! : t,
          );
          return { tabs };
        });
      },
      rename: (id, label) => {
        set((s) => ({
          tabs: s.tabs.map((t) => (t.id === id ? { ...t, label } : t)),
        }));
      },
      bindFile: (id, file) => {
        set((s) => {
          const tab = s.tabs.find((t) => t.id === id);
          if (!tab || tab.kind !== "editor" || tab.params.file === file) return s;
          return {
            tabs: s.tabs.map((t) =>
              t.id === id ? { ...t, params: { ...t.params, file } } : t,
            ),
          };
        });
      },
    }),
    {
      name: "iterion.tabs",
      storage: createJSONStorage(() => localStorage),
      partialize: (state) => ({
        tabs: state.tabs.map((t) => ({
          id: t.id,
          kind: t.kind,
          params: t.params,
          label: t.label,
          projectKey: t.projectKey,
          // Persist as dormant so cold start doesn't fetch every
          // editor file / open every run WS on page reload — see
          // onRehydrateStorage.
          hydrated: false,
        })),
        activeEditorTabId: state.activeEditorTabId,
        activeRunTabId: state.activeRunTabId,
      }),
      onRehydrateStorage: () => (state) => {
        if (!state) return;
        // Drop entries that aren't a current TabKind — an earlier
        // version of this store persisted "home" / "whats-next" / etc.
        // before those moved to sidebar-only navigation.
        state.tabs = state.tabs
          .filter((t) => t.kind === "editor" || t.kind === "run")
          // Drop legacy tabs persisted before project scoping (no
          // projectKey) — a one-time reset so they can't leak across
          // projects. New tabs are always tagged with the active project
          // on open.
          .filter((t) => t.projectKey != null)
          .map((t) => ({ ...t, hydrated: false, restored: true }));
        if (!state.tabs.some((t) => t.id === state.activeEditorTabId)) {
          state.activeEditorTabId = null;
        }
        if (!state.tabs.some((t) => t.id === state.activeRunTabId)) {
          state.activeRunTabId = null;
        }
      },
    },
  ),
);

// tabInScope is true when the tab belongs to the active project (or
// scoping is off — cloud mode, no folder). Drives the project-scoped tab
// strips so switching projects never surfaces another project's tab.
function tabInScope(t: Tab, key: string | null): boolean {
  return key == null || t.projectKey === key;
}

export function selectEditorTabs(state: TabsState): Tab[] {
  return state.tabs.filter(
    (t) => t.kind === "editor" && tabInScope(t, state.currentProjectKey),
  );
}

export function selectRunTabs(state: TabsState): Tab[] {
  return state.tabs.filter(
    (t) => t.kind === "run" && tabInScope(t, state.currentProjectKey),
  );
}
