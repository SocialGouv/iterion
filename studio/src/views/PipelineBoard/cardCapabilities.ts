import type { PipelineBoardCard } from "@/api/pipelineBoards";

import { cardReady } from "./cardPredicates";

// Ticket movement is BUTTON-driven — there is no drag & drop on this board.
// canMarkReady: stage a not-yet-ready Opened task into the launch queue, or
// retry a failed pipeline (needs attention, or a stopped one in Closed) —
// all flip the ticket to StateReady.
//
// Retrying from the needs-attention lane is also how that card's reserved
// concurrency slot is released: the ticket leaves the lane the moment it is
// restaged, so the reservation evaporates before the launch gate is even
// consulted and the card can never be refused by its own held slot.
export function canMarkReady(card: PipelineBoardCard): boolean {
  if (!card.issue_id) return false;
  if (card.column_id === "needs_attention") return card.failed === true;
  if (card.column_id === "closed") return card.failed === true;
  if (card.column_id === "opened") return card.kind === "task" && !cardReady(card);
  return false;
}

export function canUnmarkReady(card: PipelineBoardCard): boolean {
  return (
    !!card.issue_id &&
    card.column_id === "opened" &&
    card.kind === "task" &&
    card.ready === true
  );
}

export function canDeleteTicket(card: PipelineBoardCard): boolean {
  return (
    !!card.issue_id &&
    card.kind === "task" &&
    card.column_id === "opened" &&
    !cardReady(card)
  );
}

// canLaunchNow: the ticket may be dragged from Opened straight into In
// progress, overriding the admission loop's priority order. Mirrors the
// server's guards (POST .../tasks/{id}/launch) so the UI never offers a
// drag the backend would 409 — except the concurrency cap, which is live
// state the server owns and reports back.
export function canLaunchNow(card: PipelineBoardCard): boolean {
  return (
    !!card.issue_id &&
    card.kind === "task" &&
    card.column_id === "opened" &&
    !card.run_id &&
    (card.open_blocker_count ?? 0) === 0
  );
}

export function canPauseRun(card: PipelineBoardCard): boolean {
  return card.column_id === "in_progress" && !!card.run_id && card.status === "running";
}

export function canResumeRun(card: PipelineBoardCard): boolean {
  return (
    card.column_id === "in_progress" &&
    !!card.run_id &&
    card.status === "paused_operator"
  );
}

export function canResetTicket(card: PipelineBoardCard): boolean {
  if (!card.issue_id || !card.run_id) return false;
  return (
    card.column_id === "in_progress" || card.column_id === "needs_attention"
  );
}

export function canStopRun(card: PipelineBoardCard): boolean {
  return card.column_id === "in_progress" && !!card.run_id && !card.issue_id;
}

// A run status the engine will never move again on its own. Mirrors Go's
// store.RunStatus.IsTerminal() — the predicate the server's own `fresh`
// guard uses — so the menu never offers an action the backend would 409.
const TERMINAL_RUN_STATUSES = new Set([
  "finished",
  "failed",
  "failed_resumable",
  "cancelled",
]);

export function isTerminalRunStatus(status: string | undefined): boolean {
  return !!status && TERMINAL_RUN_STATUSES.has(status);
}

// canRetryFromZero: Retry, minus the ambiguity. Plain Retry only restages
// the ticket and lets whoever claims it decide what that meant — the studio
// admission loop mints a fresh run, a live `iterion dispatch` resumes the
// dead one from its checkpoint. This offers the deterministic form: the
// server drops the last-run pointer first, so neither authority can resume.
//
// Offered wherever Retry is (the two failed lanes), and only once the root
// run has settled: the server refuses while anything in the ticket's tree is
// non-terminal, and the tree it reads includes descendants this card cannot
// see — so the affordance mirrors the guard it can evaluate and the 409
// remains the authority for the rest.
export function canRetryFromZero(card: PipelineBoardCard): boolean {
  if (!card.issue_id || !card.run_id || card.failed !== true) return false;
  if (card.column_id !== "needs_attention" && card.column_id !== "closed") {
    return false;
  }
  return isTerminalRunStatus(card.status);
}

// canCloseCard: file this pipeline for good — cancel whatever is still
// alive under it and move it to Closed. It is the release valve for the
// needs-attention lane, where a card holds a concurrency slot until it is
// retried or closed.
//
// TICKET-BACKED only: closing writes a terminal ticket state, and a
// standalone run has none. Such a run also reserves nothing and the
// projection files it straight into Closed, so there is nothing to release.
// Not offered in Closed, where the card has already reached its end.
export function canCloseCard(card: PipelineBoardCard): boolean {
  return !!card.issue_id && card.column_id !== "closed";
}

export function isTicketEditable(card: PipelineBoardCard): boolean {
  if (!card.issue_id) return false;
  return (
    (card.column_id === "opened" && card.kind === "task") ||
    card.column_id === "needs_attention" ||
    (card.column_id === "closed" && card.failed === true)
  );
}
