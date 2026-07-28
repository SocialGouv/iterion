import type { RunStatus, RunSummary } from "@/api/runs";

import { labelForStatus } from "../runStatusMeta";

export const STATUS_FILTERS: Array<{ value: RunStatus | ""; label: string }> = [
  { value: "", label: "All" },
  { value: "running", label: "Running" },
  // queued sits between running and paused so the eye walks the
  // progression naturally (cloud-ready plan §F T-13).
  { value: "queued", label: "Queued" },
  { value: "paused_waiting_human", label: "Paused" },
  // Operator soft-pause has its own status; the row above is the
  // human-input variant. Keep both addressable in the filter strip so
  // an operator triaging "what's paused" can disambiguate at-a-glance.
  { value: "paused_operator", label: "Paused (operator)" },
  { value: "finished", label: "Finished" },
  { value: "failed", label: "Failed" },
  { value: "failed_resumable", label: "Failed (resumable)" },
  { value: "cancelled", label: "Cancelled" },
];

// hasFriendlyName returns true when run.name is set AND differs from
// run.id. Defensive guard against historical bugs where dispatcher-
// spawned runs aliased Name to the composite RunID (now fixed — see
// pkg/dispatcher/loop.go); legacy stores may still contain such rows.
export function hasFriendlyName(run: RunSummary): boolean {
  return Boolean(run.name) && run.name !== run.id;
}

// friendlyLabel returns the per-run instance label for the "Run"
// column. Falls back to workflow_name for legacy runs (persisted
// before the friendly-name feature shipped).
export function friendlyLabel(run: RunSummary): string {
  return hasFriendlyName(run) ? run.name! : run.workflow_name;
}

// workflowDisplay returns the label for the "Workflow" column. Returns
// "" when the value would duplicate the run id (dispatcher-spawned
// runs in some legacy paths aliased workflow_name to the composite
// run id; suppress to keep the row from showing the id twice).
export function workflowDisplay(run: RunSummary): string {
  const name = run.bundle_name || run.workflow_name;
  if (!name || name === run.id) return "";
  return name;
}

// shortRunID collapses a long run id to a glanceable prefix. Keeps
// the tooltip-attached full id within reach via `title=`. For the
// dispatcher's composite ids (e.g. `dispatcher-native_<uuid>-<seq>-<ts>`),
// we surface the prefix + the first UUID segment so two siblings
// from the same dispatcher slot are still visually distinct.
export function shortRunID(id: string): string {
  if (!id) return "";
  const dispPrefix = "dispatcher-native_";
  if (id.startsWith(dispPrefix)) {
    const tail = id.slice(dispPrefix.length);
    const dash = tail.indexOf("-");
    return `disp:${dash > 0 ? tail.slice(0, dash) : tail.slice(0, 8)}`;
  }
  return id.length > 14 ? id.slice(0, 14) + "…" : id;
}

// statusFilterLabel returns a lower-case fragment suited for the
// "No <status> runs" empty headline (matches the chip label so the
// phrasing stays consistent between the chip and the empty state).
// Exported for the corresponding unit test; not consumed elsewhere.
export function statusFilterLabel(status: RunStatus | ""): string {
  if (status === "") return "matching";
  const entry = STATUS_FILTERS.find((f) => f.value === status);
  const label = entry?.label ?? labelForStatus(status);
  return label.toLowerCase();
}

export function formatDuration(startISO: string, endISO?: string): string {
  const start = Date.parse(startISO);
  if (Number.isNaN(start)) return "";
  const end = endISO ? Date.parse(endISO) : Date.now();
  if (Number.isNaN(end)) return "";
  const ms = Math.max(0, end - start);
  const seconds = Math.round(ms / 1000);
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  const remSec = seconds % 60;
  if (minutes < 60) return `${minutes}m ${remSec}s`;
  const hours = Math.floor(minutes / 60);
  const remMin = minutes % 60;
  return `${hours}h ${remMin}m`;
}

// retryHint renders the pending automatic retry of a run parked on an
// exhausted provider quota window ("retrying in 33h"), or "" when none is
// armed.
//
// This is the whole reason the field is surfaced: a failed_resumable row
// that WILL resume and one that never will are otherwise identical, so a
// waiting run reads as a dead one and gets resumed by hand — or worse,
// written off. An instant already in the past renders as "retrying soon"
// rather than a negative duration: the sweeper polls, so "due" and
// "running again" are a minute apart.
export function retryHint(run: RunSummary, now: number = Date.now()): string {
  if (!run.retry_after) return "";
  const at = Date.parse(run.retry_after);
  if (Number.isNaN(at)) return "";
  const ms = at - now;
  if (ms <= 0) return "retrying soon";
  const minutes = Math.round(ms / 60000);
  if (minutes < 60) return `retrying in ${Math.max(1, minutes)}m`;
  const hours = Math.round(minutes / 60);
  if (hours < 48) return `retrying in ${hours}h`;
  return `retrying in ${Math.round(hours / 24)}d`;
}

// retryAttemptLabel renders the run's position in its attempt budget, for
// the tooltip beside retryHint. Empty when nothing has been attempted.
export function retryAttemptLabel(run: RunSummary): string {
  if (!run.retry_attempts) return "";
  return `automatic retry ${run.retry_attempts}`;
}
