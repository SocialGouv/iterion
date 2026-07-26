// Extracted from api/runs.ts to keep that file focused.
// Mutating run endpoints: create, cost preview, cancel/pause, watch
// subscription, fork, resume, rename.

import { ApiError } from "../client";
import { request } from "./client";
import type {
  CreateRunRequest,
  CreateRunResponse,
  ForkRunRequest,
  ForkRunResponse,
  PreviewCostResponse,
  ResumeRunRequest,
} from "./types";

// Stable wire code emitted by POST /runs/:id/resume when the current workflow
// source no longer matches the run's launch hash.
export const WORKFLOW_SOURCE_CHANGED_ERROR_CODE = "workflow_source_changed";

// isWorkflowSourceChangedError prefers the structured API contract. The prose
// fallback only applies when no error_code was supplied, preserving force
// resume against older servers and direct/plain-error test doubles.
export function isWorkflowSourceChangedError(err: unknown): boolean {
  if (err instanceof ApiError && err.errorCode !== undefined) {
    return err.errorCode === WORKFLOW_SOURCE_CHANGED_ERROR_CODE;
  }
  const message =
    err instanceof Error
      ? err.message
      : typeof err === "string"
        ? err
        : typeof (err as { message?: unknown } | null)?.message === "string"
          ? String((err as { message: string }).message)
          : "";
  return /source has changed/i.test(message);
}

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

// answerInteraction answers a pending ASYNC question (ADR-081,
// ask_user_async) — valid while the run is RUNNING or paused. The
// answer is delivered to the asking node's message queue; when it
// completes an await-paused run's pending set, the run auto-resumes
// (resumed: true).
export async function answerInteraction(
  runId: string,
  interactionId: string,
  answer: string,
): Promise<{ run_id: string; interaction_id: string; queued: boolean; resumed: boolean }> {
  return request(
    `/runs/${encodeURIComponent(runId)}/interactions/${encodeURIComponent(interactionId)}/answer`,
    { method: "POST", body: JSON.stringify({ answer }) },
  );
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

// getRunTags lists a run's operator-assigned filter/group tags (chips in
// the run header). Returns an empty array for a run with none.
export async function getRunTags(runId: string): Promise<string[]> {
  const res = await request<{ tags: string[] }>(
    `/runs/${encodeURIComponent(runId)}/tags`,
  );
  return res.tags ?? [];
}

// setRunTags replaces a run's FULL tag set (whole-list overwrite, not a
// merge). The server normalizes (trim/dedup) and enforces limits (max 32
// chars per tag, max 20 tags) — an over-limit list is a 400. Returns the
// normalized set the server persisted.
export async function setRunTags(
  runId: string,
  tags: string[],
): Promise<string[]> {
  const res = await request<{ tags: string[] }>(
    `/runs/${encodeURIComponent(runId)}/tags`,
    {
      method: "PUT",
      body: JSON.stringify({ tags }),
    },
  );
  return res.tags ?? [];
}
