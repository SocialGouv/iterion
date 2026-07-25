// Session-status vocabulary for the whats-next hook family.
//
// WhatsNextStatus is the high-level UI state machine; isEndedRunStatus
// is the pure classifier behind the runStatus → WhatsNextStatus
// promotion (terminal engine statuses map to "ended", everything else
// that has a runId maps to "active").

import type { RunStatus } from "@/api/runs";

export type WhatsNextStatus =
  | "idle"
  | "launching"
  | "active"
  | "submitting"
  | "ended";

// True for the engine statuses the session treats as "ended". Note this
// deliberately includes failed_resumable (the session shows the Resume
// affordance from the ended state) — a different set from the WS
// heartbeat's terminal statuses, which exclude it.
export function isEndedRunStatus(status: RunStatus): boolean {
  return (
    status === "finished" ||
    status === "failed" ||
    status === "cancelled" ||
    status === "failed_resumable"
  );
}
