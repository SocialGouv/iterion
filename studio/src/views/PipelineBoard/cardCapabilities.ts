import type { PipelineBoardCard } from "@/api/pipelineBoards";

import { cardReady } from "./columnFilters";

// Ticket movement is BUTTON-driven — there is no drag & drop on this board.
// canMarkReady: stage a not-yet-ready Opened task into the launch queue, or
// retry a Failed pipeline in Closed — both flip the ticket to StateReady.
export function canMarkReady(card: PipelineBoardCard): boolean {
  if (!card.issue_id) return false;
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
  return card.column_id === "in_progress" && !!card.issue_id && !!card.run_id;
}

export function canStopRun(card: PipelineBoardCard): boolean {
  return card.column_id === "in_progress" && !!card.run_id && !card.issue_id;
}

export function isTicketEditable(card: PipelineBoardCard): boolean {
  if (!card.issue_id) return false;
  return (
    (card.column_id === "opened" && card.kind === "task") ||
    (card.column_id === "closed" && card.failed === true)
  );
}
