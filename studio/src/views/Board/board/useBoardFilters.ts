// Filter/sort/group + saved-view state for the board — the whole toolbar
// ("views bar") concern in one hook: the mutable filter combo, the
// repo-first scoping, and the saved-view apply/save/delete handlers that
// snapshot or restore that combo. Kept out of index.tsx so the orchestrator
// only wires the result into <BoardFilterBar>.

import { useCallback, useState } from "react";

import { deleteView, saveView, type NativeView } from "@/api/native";
import { useActiveRepo } from "@/hooks/useActiveRepo";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { useToggleSet } from "@/hooks/useToggleSet";

import type { GroupMode, SortMode } from "../boardShared";

import { filtersFromView, viewFromFilters } from "./viewMapping";

export interface UseBoardFiltersResult {
  searchQuery: string;
  setSearchQuery: (v: string) => void;
  // `onLabelToggle` (from useToggleSet) is the single source of truth
  // for label filter toggling — used both by the top filter strip and
  // by clicking a chip on any card, so card-level chips toggle the
  // same Set the filter strip shows.
  labelFilter: Set<string>;
  onLabelToggle: (label: string) => void;
  clearLabelFilter: () => void;
  assigneeFilter: string;
  setAssigneeFilter: (v: string) => void;
  botFilter: string;
  setBotFilter: (v: string) => void;
  sortMode: SortMode;
  setSortMode: (m: SortMode) => void;
  groupMode: GroupMode;
  setGroupMode: (m: GroupMode) => void;
  /** Active repo full name when the board is repo-scoped, else null. */
  repoScope: string | null;
  includeUnlinked: boolean;
  setIncludeUnlinked: (v: boolean) => void;
  /** Currently-applied saved view's name; "" = custom/unsaved. */
  activeView: string;
  applyView: (v: NativeView | null) => void;
  onSaveView: (name: string) => void;
  onDeleteView: (name: string) => void;
  viewBusy: boolean;
  viewError: string | null;
  /** Clears search/labels/assignee/bot/group + the active view (sort kept). */
  reset: () => void;
}

export function useBoardFilters({
  refresh,
}: {
  refresh: () => Promise<void>;
}): UseBoardFiltersResult {
  const [searchQuery, setSearchQuery] = useState("");
  const {
    set: labelFilter,
    toggle: onLabelToggle,
    clear: clearLabelFilter,
    replace: replaceLabels,
  } = useToggleSet<string>();
  const [assigneeFilter, setAssigneeFilter] = useState("");
  const [botFilter, setBotFilter] = useState("");
  const [sortMode, setSortMode] = useState<SortMode>("priority");
  const [groupMode, setGroupMode] = useState<GroupMode>("none");
  // Repo-first scoping: when the sidebar has an active repo (cloud + not
  // overview), filter cards to that repo. `includeUnlinked` widens the
  // filter to also show cards with no external link (default off).
  const {
    activeRepo,
    overview: repoOverview,
    enabled: repoScopeEnabled,
  } = useActiveRepo();
  const repoScope =
    repoScopeEnabled && !repoOverview ? (activeRepo?.repo_full_name ?? null) : null;
  const [includeUnlinked, setIncludeUnlinked] = useState(false);
  // Saved views: activeView is the currently-applied preset's name ("" =
  // custom/unsaved). viewAction tracks the save/delete REST call.
  const [activeView, setActiveView] = useState("");
  const viewAction = useAsyncAction();

  // Saved-view handlers. applyView restores a preset's filter/sort/group;
  // onSaveView snapshots the current combo; onDeleteView drops one.
  const applyView = useCallback(
    (v: NativeView | null) => {
      if (!v) {
        setActiveView("");
        return;
      }
      const f = filtersFromView(v);
      setSearchQuery(f.search);
      replaceLabels(f.labels);
      setAssigneeFilter(f.assignee);
      setBotFilter(f.bot);
      setSortMode(f.sort);
      setGroupMode(f.group);
      setActiveView(v.name);
    },
    [replaceLabels],
  );
  const onSaveView = useCallback(
    (name: string) => {
      if (!name) return;
      void viewAction.run(async () => {
        await saveView(
          viewFromFilters(name, {
            search: searchQuery,
            labels: [...labelFilter],
            assignee: assigneeFilter,
            bot: botFilter,
            sort: sortMode,
            group: groupMode,
          }),
        );
        await refresh();
        setActiveView(name);
      });
    },
    [viewAction, searchQuery, labelFilter, assigneeFilter, botFilter, sortMode, groupMode, refresh],
  );
  const onDeleteView = useCallback(
    (name: string) => {
      void viewAction.run(async () => {
        await deleteView(name);
        await refresh();
        setActiveView((cur) => (cur === name ? "" : cur));
      });
    },
    [viewAction, refresh],
  );

  const reset = useCallback(() => {
    setSearchQuery("");
    clearLabelFilter();
    setAssigneeFilter("");
    setBotFilter("");
    setGroupMode("none");
    setActiveView("");
  }, [clearLabelFilter]);

  return {
    searchQuery,
    setSearchQuery,
    labelFilter,
    onLabelToggle,
    clearLabelFilter,
    assigneeFilter,
    setAssigneeFilter,
    botFilter,
    setBotFilter,
    sortMode,
    setSortMode,
    groupMode,
    setGroupMode,
    repoScope,
    includeUnlinked,
    setIncludeUnlinked,
    activeView,
    applyView,
    onSaveView,
    onDeleteView,
    viewBusy: viewAction.busy,
    viewError: viewAction.error,
    reset,
  };
}
