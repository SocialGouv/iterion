// Status → action eligibility predicates shared by the run list (inline
// Resume, bulk toolbar) and RunHeader, so the two surfaces can't drift.

import type { RunStatus } from "@/api/runs";

// Resume targets checkpoint-based restarts (Engine.Resume dispatch).
// paused_waiting_human is intentionally excluded — that state resumes
// through the answer form in the run console, not a bare resume.
export function isResumable(status: RunStatus): boolean {
  return (
    status === "failed_resumable" ||
    status === "cancelled" ||
    status === "paused_operator"
  );
}

// Bulk cancel from the list targets runs with in-flight or queued work.
// Paused runs are excluded here — cancelling one is a deliberate
// per-run gesture (RunHeader), not a bulk sweep.
export function isCancellable(status: RunStatus): boolean {
  return status === "running" || status === "queued";
}

// Delete is terminal-only — an active run must be cancelled first.
export function isDeletable(status: RunStatus): boolean {
  return (
    status === "finished" ||
    status === "failed" ||
    status === "failed_resumable" ||
    status === "cancelled"
  );
}
