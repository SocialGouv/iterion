// Toolbar strip for the board: the filter bar + the saved-views bar, wired
// from the useBoardFilters bundle. Render-only — every piece of state and
// every handler comes in through `filters`.

import type { NativeBoard } from "@/api/native";

import { BoardFilters } from "../BoardFilters";
import { ViewBar } from "../ViewBar";

import type { UseBoardFiltersResult } from "./useBoardFilters";

export function BoardFilterBar({
  filters,
  board,
  allLabels,
  allAssignees,
  allBots,
  total,
  filtered,
  hasRepoLinks,
}: {
  filters: UseBoardFiltersResult;
  board: NativeBoard;
  allLabels: string[];
  allAssignees: string[];
  allBots: string[];
  total: number;
  filtered: number;
  hasRepoLinks: boolean;
}) {
  return (
    <>
      <BoardFilters
        searchQuery={filters.searchQuery}
        labelFilter={filters.labelFilter}
        assigneeFilter={filters.assigneeFilter}
        botFilter={filters.botFilter}
        allLabels={allLabels}
        allAssignees={allAssignees}
        allBots={allBots}
        total={total}
        filtered={filtered}
        onSearchChange={filters.setSearchQuery}
        onLabelToggle={filters.onLabelToggle}
        onClearLabels={filters.clearLabelFilter}
        onAssigneeChange={filters.setAssigneeFilter}
        onBotChange={filters.setBotFilter}
        sortMode={filters.sortMode}
        onSortChange={filters.setSortMode}
        groupMode={filters.groupMode}
        onGroupChange={filters.setGroupMode}
        fieldNames={(board.fields ?? []).map((f) => f.name)}
        hasRepoLinks={hasRepoLinks}
        repoScope={filters.repoScope}
        includeUnlinked={filters.includeUnlinked}
        onIncludeUnlinkedChange={filters.setIncludeUnlinked}
        onReset={filters.reset}
      />
      <ViewBar
        views={board.views ?? []}
        activeView={filters.activeView}
        onApply={filters.applyView}
        onSave={filters.onSaveView}
        onDelete={filters.onDeleteView}
        busy={filters.viewBusy}
        error={filters.viewError}
      />
    </>
  );
}
