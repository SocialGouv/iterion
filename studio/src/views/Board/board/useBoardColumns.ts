import { useMemo } from "react";

import type { NativeBoard, NativeIssue } from "@/api/native";

import type { SortMode } from "../boardShared";

import { sortComparator } from "./boardSort";

export interface UseBoardColumnsResult {
  filteredIssues: NativeIssue[];
  byState: Map<string, NativeIssue[]>;
  flatIssueIds: string[];
  allLabels: string[];
  allAssignees: string[];
  allBots: string[];
}

// Composes the post-filter / per-column derived data for the board:
// distinct labels/assignees from the raw issues, the filtered+sorted
// per-column buckets, and the flat column-then-row issue-id sequence
// used as a 1-D coordinate space for shift-click range extension.
export function useBoardColumns({
  board,
  issues,
  searchQuery,
  labelFilter,
  assigneeFilter,
  botFilter,
  sortMode,
  repoScope = null,
  includeUnlinked = false,
}: {
  board: NativeBoard | null;
  issues: NativeIssue[];
  searchQuery: string;
  labelFilter: ReadonlySet<string>;
  assigneeFilter: string;
  botFilter: string;
  sortMode: SortMode;
  // repo-first scoping: when set (repo_full_name), keep only cards linked
  // to that repo; `includeUnlinked` widens the filter to also let cards
  // with no external link through. Null = no repo filter (overview /
  // outside cloud mode).
  repoScope?: string | null;
  includeUnlinked?: boolean;
}): UseBoardColumnsResult {
  // Distinct values exposed in the filter dropdowns. Derived from the
  // current issues list so the dropdowns track what the user actually
  // sees — including labels created on the fly by bots.
  const { allLabels, allAssignees, allBots } = useMemo(() => {
    const labels = new Set<string>();
    const assignees = new Set<string>();
    const bots = new Set<string>();
    for (const iss of issues) {
      for (const l of iss.labels ?? []) labels.add(l);
      if (iss.assignee) assignees.add(iss.assignee);
      if (iss.bot) bots.add(iss.bot);
    }
    return {
      allLabels: Array.from(labels).sort(),
      allAssignees: Array.from(assignees).sort(),
      allBots: Array.from(bots).sort(),
    };
  }, [issues]);

  // filteredIssues applies the search query + active label/assignee
  // filters to the raw issues list. Keep filtering client-side: the
  // backend's listIssues filter would force a full network round-trip
  // on every keystroke, which makes the search feel laggy on
  // multi-hundred-issue boards. Title/body substring is case-insensitive.
  const filteredIssues = useMemo(() => {
    const q = searchQuery.trim().toLowerCase();
    const labels = labelFilter;
    const assignee = assigneeFilter.trim();
    const bot = botFilter.trim();
    const scope = repoScope ?? "";
    if (!q && labels.size === 0 && !assignee && !bot && !scope) return issues;
    return issues.filter((iss) => {
      if (q) {
        const hay =
          (iss.title ?? "") + "\t" + (iss.body ?? "") + "\t" + iss.id;
        if (!hay.toLowerCase().includes(q)) return false;
      }
      if (labels.size > 0) {
        const have = new Set(iss.labels ?? []);
        for (const l of labels) {
          if (!have.has(l)) return false;
        }
      }
      if (assignee && iss.assignee !== assignee) return false;
      if (bot && iss.bot !== bot) return false;
      if (scope) {
        const linkedRepo = iss.external?.repo ?? "";
        if (linkedRepo === scope) return true;
        if (includeUnlinked && !linkedRepo) return true;
        return false;
      }
      return true;
    });
  }, [issues, searchQuery, labelFilter, assigneeFilter, botFilter, repoScope, includeUnlinked]);

  // Group filtered issues by state for column rendering. Issues whose
  // state does not appear on the board land in an "unmapped" bucket so
  // they are not silently lost when the operator renames a state.
  const byState = useMemo(() => {
    const m = new Map<string, NativeIssue[]>();
    if (!board) return m;
    for (const s of board.states) m.set(s.name, []);
    m.set("__unmapped__", []);
    for (const iss of filteredIssues) {
      const bucket = m.has(iss.state) ? iss.state : "__unmapped__";
      m.get(bucket)!.push(iss);
    }
    const cmp = sortComparator(sortMode);
    for (const list of m.values()) {
      list.sort(cmp);
    }
    return m;
  }, [board, filteredIssues, sortMode]);

  // Flat issue-id sequence in column-then-row order. Used as the
  // 1-D coordinate space for shift-click range extension across
  // columns: anchor and clicked card are indexed here, everything
  // between them is added to the selection.
  const flatIssueIds = useMemo(() => {
    const flat: string[] = [];
    if (!board) return flat;
    for (const s of board.states) {
      for (const iss of byState.get(s.name) ?? []) flat.push(iss.id);
    }
    for (const iss of byState.get("__unmapped__") ?? []) flat.push(iss.id);
    return flat;
  }, [board, byState]);

  return {
    filteredIssues,
    byState,
    flatIssueIds,
    allLabels,
    allAssignees,
    allBots,
  };
}
