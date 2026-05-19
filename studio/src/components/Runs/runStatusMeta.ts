import type { BadgeVariant } from "@/components/ui/Badge";
import type { RunStatus } from "@/api/runs";

export const STATUS_VARIANT: Record<RunStatus, BadgeVariant> = {
  running: "info",
  paused_waiting_human: "warning",
  // Operator pause uses "info-soft" semantics — distinct from
  // paused_waiting_human's amber so a human glancing at the canvas
  // can tell at-a-glance whether the run is waiting on a human form
  // ("warning"/amber = action required) or merely sitting because the
  // operator hit Pause ("info" cyan/teal = no action required).
  paused_operator: "info",
  finished: "success",
  failed: "danger",
  failed_resumable: "danger",
  cancelled: "neutral",
  queued: "neutral",
};

export function labelForStatus(s: RunStatus): string {
  switch (s) {
    case "paused_waiting_human":
      return "Paused (input)";
    case "paused_operator":
      return "Paused (operator)";
    case "failed_resumable":
      return "Failed (resumable)";
    case "queued":
      return "Queued";
    default:
      return s;
  }
}

export function isActiveStatus(s: RunStatus): boolean {
  return s === "running" || s === "queued";
}
