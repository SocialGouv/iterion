import type { ExecStatus, RunStatus as ApiRunStatus } from "@/api/runs";

// Unified status taxonomy spanning both run-level and exec-level
// statuses. "none" is the sentinel for IR nodes never visited in this
// run. The Workflow view holds it; the per-execution rows never do.
export type UnifiedStatus = ExecStatus | ApiRunStatus | "none";

export interface StatusClasses {
  bg: string;
  border: string;
  text: string;
  // BadgeVariant from the ui/Badge component, so callers don't have to
  // map twice. Kept narrow on purpose to avoid a circular import.
  badgeVariant: "neutral" | "info" | "warning" | "danger" | "success" | "accent";
  // Glyph used by IRNode pips and StatusBadge.
  glyph: string;
  label: string;
}

const RUNNING: StatusClasses = {
  bg: "bg-info-soft animate-pulse",
  border: "border-info",
  text: "text-fg-default",
  badgeVariant: "info",
  glyph: "▶",
  label: "Running",
};

const FINISHED: StatusClasses = {
  bg: "bg-success-soft",
  border: "border-success/60",
  text: "text-fg-default",
  badgeVariant: "success",
  glyph: "✓",
  label: "Finished",
};

const FAILED: StatusClasses = {
  bg: "bg-danger-soft",
  border: "border-danger/60",
  text: "text-fg-default",
  badgeVariant: "danger",
  glyph: "✗",
  label: "Failed",
};

const FAILED_RESUMABLE: StatusClasses = {
  ...FAILED,
  label: "Failed (resumable)",
};

const PAUSED: StatusClasses = {
  bg: "bg-warning-soft",
  border: "border-warning/60",
  text: "text-fg-default",
  badgeVariant: "warning",
  glyph: "⏸",
  label: "Paused",
};

const CANCELLED: StatusClasses = {
  bg: "bg-surface-2",
  border: "border-border-default",
  text: "text-fg-muted",
  badgeVariant: "neutral",
  glyph: "⊘",
  label: "Cancelled",
};

const SKIPPED: StatusClasses = {
  bg: "bg-surface-2",
  border: "border-border-default",
  text: "text-fg-subtle",
  badgeVariant: "neutral",
  glyph: "·",
  label: "Skipped",
};

const NONE: StatusClasses = {
  bg: "bg-surface-1",
  border: "border-border-default",
  text: "text-fg-subtle",
  badgeVariant: "neutral",
  glyph: "○",
  label: "Idle",
};

const TABLE: Record<UnifiedStatus, StatusClasses> = {
  running: RUNNING,
  finished: FINISHED,
  failed: FAILED,
  failed_resumable: FAILED_RESUMABLE,
  paused_waiting_human: PAUSED,
  cancelled: CANCELLED,
  skipped: SKIPPED,
  none: NONE,
};

export function statusClasses(status: UnifiedStatus): StatusClasses {
  return TABLE[status] ?? NONE;
}

// Backwards-compatible alias kept for the Workflow view, which has been
// passing a narrowed type. The implementation is the same lookup.
export type RunStatus = UnifiedStatus;
