// Extracted from api/runs.ts to keep that file focused.
// Mutating run endpoints: create, cost preview, cancel/pause, watch
// subscription, fork, resume, rename.

import { request } from "./client";
import type {
  CreateRunRequest,
  CreateRunResponse,
  ForkRunRequest,
  ForkRunResponse,
  PreviewCostResponse,
  ResumeRunRequest,
} from "./types";

export async function createRun(req: CreateRunRequest): Promise<CreateRunResponse> {
  return request("/runs", {
    method: "POST",
    body: JSON.stringify(req),
  });
}

export async function previewRunCost(req: {
  file_path?: string;
  source?: string;
}): Promise<PreviewCostResponse> {
  return request("/runs/preview-cost", {
    method: "POST",
    body: JSON.stringify(req),
  });
}

export async function cancelRun(
  runId: string,
): Promise<{ run_id: string; status: string }> {
  return request(`/runs/${encodeURIComponent(runId)}/cancel`, { method: "POST" });
}

// deleteRun permanently removes a run and ALL of its data (events,
// artifacts, interactions, attachments). Irreversible. Tenant-scoped
// server-side: a 404 means the run is gone or outside your team's scope.
// Returns void (the server answers 204 No Content).
export async function deleteRun(runId: string): Promise<void> {
  await request<void>(`/runs/${encodeURIComponent(runId)}`, { method: "DELETE" });
}

// pauseRun requests a soft, operator-initiated pause. The engine
// interrupts at the next safe boundary (top of execLoop, between LLM
// turns inside an agent), saves a checkpoint, transitions to
// paused_operator, and emits run_paused with reason=operator — the
// run is resumable like a cancelled one. 409 means the run isn't held
// in this process (terminal, or running in cloud) — RunHeader hides
// the button in those cases but the API is defensive against double-
// clicks racing with status changes.
export async function pauseRun(
  runId: string,
): Promise<{ run_id: string; status: string }> {
  return request(`/runs/${encodeURIComponent(runId)}/pause`, { method: "POST" });
}

// addWatch subscribes a run to a native-kanban issue (MVP3b) so the
// server-side watch coordinator forwards that issue's future board
// transitions to the run as queued messages. Returns the run's full
// subscription set after the mutation.
export async function addWatch(
  runId: string,
  issueId: string,
): Promise<{ run_id: string; watched_issue_ids: string[] }> {
  return request(
    `/runs/${encodeURIComponent(runId)}/watch/${encodeURIComponent(issueId)}`,
    { method: "POST" },
  );
}

// removeWatch unsubscribes a run from a native-kanban issue.
export async function removeWatch(
  runId: string,
  issueId: string,
): Promise<{ run_id: string; watched_issue_ids: string[] }> {
  return request(
    `/runs/${encodeURIComponent(runId)}/watch/${encodeURIComponent(issueId)}`,
    { method: "DELETE" },
  );
}

// forkRun creates a new run that resumes from a prior turn of the
// parent. The new run starts in cancelled status with a synthetic
// checkpoint; the caller posts /resume on it to actually execute.
// The studio's ForkDialog opens a new run tab on the returned id and
// (by default) auto-navigates to it.
export async function forkRun(
  runId: string,
  req: ForkRunRequest,
): Promise<ForkRunResponse> {
  return request(`/runs/${encodeURIComponent(runId)}/fork`, {
    method: "POST",
    body: JSON.stringify(req),
  });
}

export async function resumeRun(
  runId: string,
  req: ResumeRunRequest = {},
): Promise<CreateRunResponse> {
  return request(`/runs/${encodeURIComponent(runId)}/resume`, {
    method: "POST",
    body: JSON.stringify(req),
  });
}

// renameRun updates a run's friendly Name without touching its id —
// callers keep their per-runId stores, tabs, deep links etc. The
// server is the source of truth; refetch the snapshot (or rely on the
// next event-stream push) to surface the change.
export async function renameRun(
  runId: string,
  name: string,
): Promise<{ run_id: string; name: string }> {
  return request(`/runs/${encodeURIComponent(runId)}/rename`, {
    method: "POST",
    body: JSON.stringify({ name }),
  });
}
