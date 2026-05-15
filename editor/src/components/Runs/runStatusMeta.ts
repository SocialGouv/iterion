import type { BadgeVariant } from "@/components/ui/Badge";
import type { RunStatus } from "@/api/runs";

export const STATUS_VARIANT: Record<RunStatus, BadgeVariant> = {
  running: "info",
  paused_waiting_human: "warning",
  finished: "success",
  failed: "danger",
  failed_resumable: "danger",
  cancelled: "neutral",
  queued: "neutral",
};

export function labelForStatus(s: RunStatus): string {
  switch (s) {
    case "paused_waiting_human":
      return "Paused";
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
