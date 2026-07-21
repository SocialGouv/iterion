// Per-column filtering for the /pipelines board. Distinct from filters.ts
// (the GLOBAL search/bot/label bar that narrows the whole board): these
// controls live in a single column's header and narrow only that column —
// the Todo lane by readiness, the Closed lane by success/failure outcome.
// They compose ON TOP of the global filter (global runs first, per-column
// second). Pure functions here so they can be unit-tested and reused by the
// card badges without a circular import back into PipelineColumns.

import type { PipelineBoardCard } from "@/api/pipelineBoards";

// cardReady reports whether a Todo card is cleared to advance to In progress:
// staged (server `ready` = StateReady) or already queued for a concurrency
// slot, AND hard deps are satisfied (no open blockers / waiting_deps).
// Drives the Ready badge and the Todo column's "Ready only" filter.
export function cardReady(card: PipelineBoardCard): boolean {
  if ((card.open_blocker_count ?? 0) > 0) return false;
  if (card.issue_state === "waiting_deps") return false;
  if (
    card.launch_blocked_reason === "open_blockers" ||
    card.launch_blocked_reason === "waiting_deps" ||
    card.launch_blocked_reason === "blocker_labels"
  ) {
    return false;
  }
  return card.ready === true || card.status === "queued";
}

// closedOutcome splits the Closed lane: a failed / cancelled / resumable run
// is "failed"; everything else there finished successfully. Drives the Closed
// column's outcome filter + the Success/Failed badge.
export function closedOutcome(card: PipelineBoardCard): "success" | "failed" {
  return card.failed === true ? "failed" : "success";
}

export type TodoReadinessFilter = "all" | "ready" | "draft";
export type ClosedOutcomeFilter = "all" | "success" | "failed";

export interface ColumnFilterState {
  // Todo lane: show all, only ready (cleared to launch), or only drafts.
  todoReadiness: TodoReadinessFilter;
  // Closed lane: show all, only successes, or only failures.
  closedOutcome: ClosedOutcomeFilter;
}

// Factory (not a shared constant) so a reset can never alias prior state.
export function emptyColumnFilters(): ColumnFilterState {
  return { todoReadiness: "all", closedOutcome: "all" };
}

// columnFilterActive reports whether the given column currently has a
// non-default per-column filter applied (drives the "N of M" header hint).
export function columnFilterActive(
  columnId: string,
  state: ColumnFilterState,
): boolean {
  if (columnId === "opened") return state.todoReadiness !== "all";
  if (columnId === "closed") return state.closedOutcome !== "all";
  return false;
}

// applyColumnFilter narrows a single column's already-globally-filtered cards
// by its per-column control. Columns without a control (in_progress, and any
// future lane) are returned unchanged.
export function applyColumnFilter(
  columnId: string,
  cards: PipelineBoardCard[],
  state: ColumnFilterState,
): PipelineBoardCard[] {
  if (columnId === "opened" && state.todoReadiness !== "all") {
    const wantReady = state.todoReadiness === "ready";
    return cards.filter((card) => cardReady(card) === wantReady);
  }
  if (columnId === "closed" && state.closedOutcome !== "all") {
    return cards.filter((card) => closedOutcome(card) === state.closedOutcome);
  }
  return cards;
}
