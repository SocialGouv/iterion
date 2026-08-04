import type { PipelineBoardCard } from "@/api/pipelineBoards";

import {
  canCloseCard,
  canDeleteTicket,
  canMarkReady,
  canPauseRun,
  canResetTicket,
  canResumeRun,
  canStopRun,
  canUnmarkReady,
  isTicketEditable,
} from "./cardCapabilities";
import { cardBlocked } from "./cardPredicates";

export type PrimaryKind =
  | "mark_ready"
  | "unmark_ready"
  | "view_deps"
  | "answer_review"
  | "resume"
  | "open_run"
  | "details"
  | "retry";

export interface PrimaryAction {
  kind: PrimaryKind;
  label: string;
  danger?: boolean;
}

export type MenuItemKind =
  | PrimaryKind
  | "edit"
  | "delete"
  | "pause"
  | "stop"
  | "reset"
  | "close"
  | "full_page"
  | "edit_bot"
  | "details";

export interface MenuItem {
  kind: MenuItemKind;
  label: string;
  danger?: boolean;
}

export function resolvePrimaryAction(card: PipelineBoardCard): PrimaryAction {
  if (card.column_id === "opened") {
    if (cardBlocked(card)) {
      return { kind: "view_deps", label: "View deps" };
    }
    if (canUnmarkReady(card)) {
      return { kind: "unmark_ready", label: "Unmark ready" };
    }
    if (canMarkReady(card)) {
      return { kind: "mark_ready", label: "Mark ready" };
    }
    return { kind: "details", label: "Details" };
  }

  if (card.column_id === "in_progress") {
    if ((card.pending_reviews?.length ?? 0) > 0) {
      return { kind: "answer_review", label: "Answer" };
    }
    if (canResumeRun(card)) {
      return { kind: "resume", label: "Resume" };
    }
    if (card.run_id) {
      return { kind: "open_run", label: "Open run" };
    }
    return { kind: "details", label: "Details" };
  }

  if (card.column_id === "needs_attention") {
    // Retry is the primary, NOT Resume — even for a resumable run. Resume
    // re-enters a run without passing through the concurrency gate (the
    // launch queue only admits fresh root launches), so making it the
    // one-click action would routinely push true concurrency past the cap.
    // Retry goes through the gate and consumes this card's own reservation.
    if (canMarkReady(card)) {
      return { kind: "retry", label: "Retry" };
    }
    if (card.run_id) {
      return { kind: "open_run", label: "Open run" };
    }
    return { kind: "details", label: "Details" };
  }

  if (card.failed && canMarkReady(card)) {
    return { kind: "retry", label: "Retry" };
  }
  return { kind: "details", label: "Details" };
}

/** Secondary actions for the ⋯ menu (excludes the primary kind). */
export function resolveMenuItems(
  card: PipelineBoardCard,
  primary: PrimaryKind,
): MenuItem[] {
  const items: MenuItem[] = [];
  const add = (item: MenuItem) => {
    if (item.kind === primary) return;
    if (items.some((i) => i.kind === item.kind)) return;
    items.push(item);
  };

  if (card.column_id === "opened") {
    if (canMarkReady(card) && cardBlocked(card)) {
      add({ kind: "mark_ready", label: "Mark ready (park if deps open)" });
    }
    if (canUnmarkReady(card)) {
      add({ kind: "unmark_ready", label: "Unmark ready" });
    }
    if (isTicketEditable(card)) {
      add({ kind: "edit", label: "Edit" });
    }
    if (canDeleteTicket(card)) {
      add({ kind: "delete", label: "Delete", danger: true });
    }
    // "Edit bot" opens the BOT's main.bot in the editor — distinct from
    // "Edit", which edits this TICKET. Only offered when the card names a
    // bot; whether that bot is an editable bundle is resolved at render
    // time against the catalog (a loose .bot has no rel_path).
    if (card.bot_id) {
      add({ kind: "edit_bot", label: "Edit bot" });
    }
  }

  if (card.column_id === "in_progress") {
    if (canPauseRun(card)) add({ kind: "pause", label: "Pause" });
    if (canResumeRun(card)) add({ kind: "resume", label: "Resume" });
    if (canResetTicket(card)) add({ kind: "reset", label: "Reset", danger: true });
    if (canStopRun(card)) add({ kind: "stop", label: "Stop", danger: true });
    if (card.run_id) add({ kind: "open_run", label: "Open run console" });
  }

  if (card.column_id === "needs_attention") {
    if (canMarkReady(card)) add({ kind: "retry", label: "Retry" });
    // Resume picks the run back up at its checkpoint instead of re-running
    // from zero — only meaningful when the engine actually saved one.
    if (card.status === "failed_resumable" && card.run_id) {
      add({ kind: "resume", label: "Resume from checkpoint" });
    }
    if (canResetTicket(card)) add({ kind: "reset", label: "Reset", danger: true });
    if (isTicketEditable(card)) add({ kind: "edit", label: "Edit" });
    if (card.bot_id) add({ kind: "edit_bot", label: "Edit bot" });
    if (card.run_id) add({ kind: "open_run", label: "Open run console" });
  }

  if (card.column_id === "closed") {
    if (card.failed && canMarkReady(card)) {
      add({ kind: "retry", label: "Retry" });
    }
    if (isTicketEditable(card)) add({ kind: "edit", label: "Edit" });
    if (card.run_id) add({ kind: "open_run", label: "Open run console" });
  }

  // Close is offered on every non-terminal lane. It is the only way to end a
  // TICKET-backed pipeline for good — Stop is run-only, and Reset restages
  // the ticket so the admission loop relaunches it immediately. In the
  // needs-attention lane it is also what releases the card's reserved slot.
  if (canCloseCard(card)) {
    add({ kind: "close", label: "Close", danger: true });
  }

  add({ kind: "details", label: "Details" });
  add({ kind: "full_page", label: "Open full page" });

  return items;
}

export function isCardSelectable(card: PipelineBoardCard): boolean {
  return card.column_id === "opened" && !!card.issue_id && card.kind === "task";
}
