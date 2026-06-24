import { useMemo } from "react";

import type { NativeBoard, NativeIssue } from "@/api/native";

import { LANE_NONE, type GroupMode, type SortMode } from "../boardShared";

import { sortComparator } from "./boardSort";

// A swimlane: one horizontal band of the board, keyed by the grouping
// dimension's value. `byState` mirrors useBoardColumns' per-column buckets
// (board states + the "__unmapped__" fallback) scoped to this lane.
export interface Lane {
  key: string;
  label: string;
  byState: Map<string, NativeIssue[]>;
  count: number;
}

// laneKeysFor returns the lane(s) an issue belongs to under groupMode.
// Most dimensions yield one lane; "label" yields one per label (an issue
// with several labels appears in each), or LANE_NONE when it has none.
function laneKeysFor(iss: NativeIssue, groupMode: GroupMode): string[] {
  if (groupMode === "assignee") {
    return [iss.assignee?.trim() || LANE_NONE];
  }
  if (groupMode === "priority") {
    return [`P${iss.priority ?? 0}`];
  }
  if (groupMode === "label") {
    const labels = iss.labels ?? [];
    return labels.length > 0 ? labels : [LANE_NONE];
  }
  if (groupMode.startsWith("field:")) {
    const name = groupMode.slice("field:".length);
    const v = iss.fields?.[name];
    if (v === undefined || v === null || v === "") return [LANE_NONE];
    return [String(v)];
  }
  return [LANE_NONE];
}

// laneLabel renders a lane key for display.
function laneLabel(key: string, groupMode: GroupMode): string {
  if (key === LANE_NONE) {
    if (groupMode === "assignee") return "Unassigned";
    if (groupMode === "label") return "No label";
    return "—";
  }
  if (groupMode === "assignee") return `@${key}`;
  return key;
}

// laneSortValue orders lanes: priority descending numeric; everything else
// alphabetical; the LANE_NONE bucket always sorts last.
function compareLaneKeys(a: string, b: string, groupMode: GroupMode): number {
  if (a === LANE_NONE) return 1;
  if (b === LANE_NONE) return -1;
  if (groupMode === "priority") {
    const na = Number(a.replace(/^P/, "")) || 0;
    const nb = Number(b.replace(/^P/, "")) || 0;
    return nb - na;
  }
  return a.localeCompare(b);
}

// useSwimlanes groups filteredIssues into horizontal lanes for the board's
// swimlane view. Returns null for groupMode "none" (the caller renders the
// flat board). Card buckets within each lane are sorted by sortMode, the
// same as useBoardColumns, so the two views stay visually consistent.
export function useSwimlanes({
  board,
  filteredIssues,
  groupMode,
  sortMode,
}: {
  board: NativeBoard | null;
  filteredIssues: NativeIssue[];
  groupMode: GroupMode;
  sortMode: SortMode;
}): Lane[] | null {
  return useMemo(() => {
    if (!board || groupMode === "none") return null;
    const stateNames = board.states.map((s) => s.name);
    const lanes = new Map<string, Map<string, NativeIssue[]>>();

    const ensureLane = (key: string) => {
      let m = lanes.get(key);
      if (!m) {
        m = new Map<string, NativeIssue[]>();
        for (const n of stateNames) m.set(n, []);
        m.set("__unmapped__", []);
        lanes.set(key, m);
      }
      return m;
    };

    for (const iss of filteredIssues) {
      for (const key of laneKeysFor(iss, groupMode)) {
        const m = ensureLane(key);
        const bucket = m.has(iss.state) ? iss.state : "__unmapped__";
        m.get(bucket)!.push(iss);
      }
    }

    const cmp = sortComparator(sortMode);
    const out: Lane[] = [];
    for (const [key, byState] of lanes) {
      let count = 0;
      for (const list of byState.values()) {
        list.sort(cmp);
        count += list.length;
      }
      out.push({ key, label: laneLabel(key, groupMode), byState, count });
    }
    out.sort((a, b) => compareLaneKeys(a.key, b.key, groupMode));
    return out;
  }, [board, filteredIssues, groupMode, sortMode]);
}
