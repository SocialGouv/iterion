import type { ExecStatus } from "@/api/runs";

// Sentinel for IR nodes that haven't been visited yet in this run.
// Used by the Workflow view; the Execution view never holds "none"
// because every execution row has a real status.
export type RunStatus = ExecStatus | "none";

export interface StatusClasses {
  bg: string;
  border: string;
  text: string;
}

const TABLE: Record<RunStatus, StatusClasses> = {
  running: {
    bg: "bg-info-soft animate-pulse",
    border: "border-info",
    text: "text-fg-default",
  },
  finished: {
    bg: "bg-success-soft",
    border: "border-success/60",
    text: "text-fg-default",
  },
  failed: {
    bg: "bg-danger-soft",
    border: "border-danger/60",
    text: "text-fg-default",
  },
  paused_waiting_human: {
    bg: "bg-warning-soft",
    border: "border-warning/60",
    text: "text-fg-default",
  },
  skipped: {
    bg: "bg-surface-2",
    border: "border-border-default",
    text: "text-fg-subtle",
  },
  none: {
    bg: "bg-surface-1",
    border: "border-border-default",
    text: "text-fg-subtle",
  },
};

export function statusClasses(status: RunStatus): StatusClasses {
  return TABLE[status] ?? TABLE.none;
}
