import type { PipelineBoardCard } from "@/api/pipelineBoards";

import {
  canDeleteTicket,
  canMarkReady,
  canPauseRun,
  canResetTicket,
  canResumeRun,
  canStopRun,
  canUnmarkReady,
  isTicketEditable,
} from "./cardCapabilities";
import { hasOpenDeps } from "./queueSummary";

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
    if (hasOpenDeps(card)) {
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
    if (canMarkReady(card) && hasOpenDeps(card)) {
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

  if (card.column_id === "closed") {
    if (card.failed && canMarkReady(card)) {
      add({ kind: "retry", label: "Retry" });
    }
    if (isTicketEditable(card)) add({ kind: "edit", label: "Edit" });
    if (card.run_id) add({ kind: "open_run", label: "Open run console" });
  }

  add({ kind: "details", label: "Details" });
  add({ kind: "full_page", label: "Open full page" });

  return items;
}

export function isCardSelectable(card: PipelineBoardCard): boolean {
  return card.column_id === "opened" && !!card.issue_id && card.kind === "task";
}
