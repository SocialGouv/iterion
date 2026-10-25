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
      return "Paused — input needed";
    case "paused_operator":
      return "Paused — operator";
    case "failed_resumable":
      return "Failed — resumable";
    case "queued":
      return "Queued";
    default:
      return s;
  }
}

export function isActiveStatus(s: RunStatus): boolean {
  return s === "running" || s === "queued";
}

// STATUS_ORDER fixes the tally's read-out order, so two chips with the
// same census always read the same sentence whatever order the child
// runs arrived in.
const STATUS_ORDER: RunStatus[] = [
  "running",
  "queued",
  "paused_waiting_human",
  "paused_operator",
  "finished",
  "failed",
  "failed_resumable",
  "cancelled",
];

// subRunStatusSummary enumerates a group of sub-run statuses as text —
// the second channel the status dots cannot carry alone (RGAA 3.1:
// information must not ride on colour only). Plain text also survives
// stylesheets-off (RGAA 10.2) and reaches the accessible name as
// content, not as an override. Rendered through sr-only beside the
// dots it narrates.
export function subRunStatusSummary(statuses: RunStatus[]): string {
  const tally = new Map<RunStatus, number>();
  for (const s of statuses) tally.set(s, (tally.get(s) ?? 0) + 1);
  const parts = STATUS_ORDER.filter((s) => tally.has(s)).map(
    (s) => `${tally.get(s)} ${labelForStatus(s).toLowerCase()}`,
  );
  const n = statuses.length;
  const head = `${n} sub-run${n === 1 ? "" : "s"}`;
  return parts.length > 0 ? `${head}: ${parts.join(", ")}` : head;
}
