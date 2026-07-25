import { Button } from "@/components/ui/Button";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";

import { BotFilterSelect } from "@/components/shared/BotFilterSelect";
import { LabelFilter } from "@/components/shared/LabelFilterPopover";

import {
  BASE_GROUP_OPTIONS,
  GROUP_MODE_REPO,
  groupModeForField,
  SORT_OPTIONS,
  type GroupMode,
  type SortMode,
} from "./boardShared";

export function BoardFilters({
  searchQuery,
  labelFilter,
  assigneeFilter,
  botFilter,
  allLabels,
  allAssignees,
  allBots,
  total,
  filtered,
  onSearchChange,
  onLabelToggle,
  onClearLabels,
  onAssigneeChange,
  onBotChange,
  sortMode,
  onSortChange,
  groupMode,
  onGroupChange,
  fieldNames,
  hasRepoLinks,
  onReset,
}: {
  searchQuery: string;
  labelFilter: Set<string>;
  assigneeFilter: string;
  botFilter: string;
  allLabels: string[];
  allAssignees: string[];
  allBots: string[];
  total: number;
  filtered: number;
  onSearchChange: (v: string) => void;
  onLabelToggle: (l: string) => void;
  onClearLabels: () => void;
  onAssigneeChange: (v: string) => void;
  onBotChange: (v: string) => void;
  sortMode: SortMode;
  onSortChange: (m: SortMode) => void;
  groupMode: GroupMode;
  onGroupChange: (m: GroupMode) => void;
  fieldNames: string[];
  // hasRepoLinks: true when at least one card is forge-linked, so the
  // "Repository" swimlane grouping is worth offering.
  hasRepoLinks: boolean;
  onReset: () => void;
}) {
  const filtersActive =
    searchQuery.trim() !== "" ||
    labelFilter.size > 0 ||
    assigneeFilter !== "" ||
    botFilter !== "";
  return (
    <div className="px-3 py-2 border-b border-border-default bg-surface-1 flex flex-wrap items-center gap-2 text-xs">
      <div className="min-w-[200px] flex-shrink-0">
        <Input
          type="search"
          value={searchQuery}
          onChange={(e) => onSearchChange(e.target.value)}
          placeholder="Search title / body / id…"
          aria-label="Search issues"
        />
      </div>
      {allAssignees.length > 0 && (
        <div className="w-auto">
          <Select
            value={assigneeFilter}
            onChange={(e) => onAssigneeChange(e.target.value)}
            aria-label="Filter by assignee"
          >
            <option value="">All assignees</option>
            {allAssignees.map((a) => (
              <option key={a} value={a}>
                @{a}
              </option>
            ))}
          </Select>
        </div>
      )}
      {allBots.length > 0 && (
        <BotFilterSelect
          value={botFilter}
          allBots={allBots}
          onChange={onBotChange}
          ariaLabel="Filter by bot"
        />
      )}
      {allLabels.length > 0 && (
        <LabelFilter
          allLabels={allLabels}
          selected={labelFilter}
          onToggle={onLabelToggle}
          onClear={onClearLabels}
        />
      )}
      <label htmlFor="board-sort-select" className="flex items-center gap-1 text-fg-muted">
        Sort
        <Select
          id="board-sort-select"
          value={sortMode}
          onChange={(e) => onSortChange(e.target.value as SortMode)}
          title="Order cards within each column"
        >
          {SORT_OPTIONS.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </Select>
      </label>
      <label htmlFor="board-group-select" className="flex items-center gap-1 text-fg-muted">
        Group
        <Select
          id="board-group-select"
          value={groupMode}
          onChange={(e) => onGroupChange(e.target.value)}
          title="Split the board into horizontal swimlanes"
        >
          {BASE_GROUP_OPTIONS.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
          {hasRepoLinks && (
            <option value={GROUP_MODE_REPO}>Repository</option>
          )}
          {fieldNames.map((n) => (
            <option key={groupModeForField(n)} value={groupModeForField(n)}>
              Field: {n}
            </option>
          ))}
        </Select>
      </label>
      <span className="ml-auto text-fg-muted">
        {filtersActive ? `${filtered} / ${total}` : `${total} issue${total === 1 ? "" : "s"}`}
      </span>
      {filtersActive && (
        <Button variant="ghost" size="sm" onClick={onReset}>
          reset
        </Button>
      )}
    </div>
  );
}
