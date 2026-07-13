import type { NativeView } from "@/api/native";

import type { GroupMode, SortMode } from "../boardShared";

// FilterState is the board's current filter/sort/group combo — the
// mutable UI state a saved View snapshots and restores. Kept as a plain
// value type so the view<->state mapping is pure and unit-testable.
export interface FilterState {
  search: string;
  labels: string[];
  assignee: string;
  bot: string;
  sort: SortMode;
  group: GroupMode;
}

// viewFromFilters snapshots the current filter combo into a NativeView
// for persistence. Empty fields are dropped (omitted) so board.json stays
// compact and matches the Go View's `omitempty` encoding.
export function viewFromFilters(name: string, s: FilterState): NativeView {
  return {
    name,
    search: s.search || undefined,
    labels: s.labels.length > 0 ? s.labels : undefined,
    assignee: s.assignee || undefined,
    bot: s.bot || undefined,
    sort: s.sort,
    group_by: s.group,
  };
}

// filtersFromView restores a saved View into a FilterState, applying the
// same defaults the board uses for an empty/unset preset.
export function filtersFromView(v: NativeView): FilterState {
  return {
    search: v.search ?? "",
    labels: v.labels ?? [],
    assignee: v.assignee ?? "",
    bot: v.bot ?? "",
    sort: (v.sort as SortMode) || "priority",
    group: (v.group_by as GroupMode) || "none",
  };
}
