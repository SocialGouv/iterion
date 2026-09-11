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
  RewindRunRequest,
  RewindRunResponse,
  ResumeRunRequest,
} from "./types";

// Stable wire code emitted by POST /runs/:id/resume when the current workflow
// source no longer matches the run's launch hash.
export const WORKFLOW_SOURCE_CHANGED_ERROR_CODE = "workflow_source_changed";
export const ARTIFACT_CONTRACT_INCOMPATIBLE_ERROR_CODE =
  "artifact_contract_incompatible";

// isWorkflowSourceChangedError prefers the structured API contract. The prose
// fallback only applies when no error_code was supplied, preserving force
// resume against servers predating workflow_source_changed and direct/plain
// error test doubles. Match the old server's canonical phrase narrowly so an
// unrelated proxy message containing "source has changed" cannot opt into
// force-resume.
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
  return /workflow source has changed/i.test(message);
}

// Both structured refusals are recoverable through the same explicit
// operator gesture: retrying the exact resume request with force enabled.
// Keep this separate from isWorkflowSourceChangedError so callers that need
// the narrower diagnostic classification do not silently change semantics.
export function isForceResumeRequiredError(err: unknown): boolean {
  if (err instanceof ApiError && err.errorCode !== undefined) {
    return (
      err.errorCode === WORKFLOW_SOURCE_CHANGED_ERROR_CODE ||
      err.errorCode === ARTIFACT_CONTRACT_INCOMPATIBLE_ERROR_CODE
    );
  }
  return isWorkflowSourceChangedError(err);
}

// Stable wire code emitted by POST /runs/:id/resume (and the WS answer path)
// when the run's status no longer admits a resume. A parked chat gate has two
// legitimate resumers — the operator, and the assistant-watch coordinator
// delivering a host event — so losing that race is a routing signal, not a
// failure to show. See pkg/server/runs_launch.go.
export const RUN_NOT_RESUMABLE_ERROR_CODE = "run_not_resumable";

// isRunNotResumableError is deliberately code-only, with no prose fallback:
// unlike workflow_source_changed there is no legacy phrasing to stay
// compatible with, and guessing from a message would risk re-routing an
// operator's answer into a queued message on an unrelated error.
export function isRunNotResumableError(err: unknown): boolean {
  return err instanceof ApiError && err.errorCode === RUN_NOT_RESUMABLE_ERROR_CODE;
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
  permission?: string;
  backend?: string;
  backend_names?: string[];
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

export interface AssistantRunWatch {
  id: string;
  /** Present when this response proves that an ancestor watch covers the requested run. */
  covered_run_id?: string;
  target_run_id: string;
  assistant_run_id: string;
  mode: "diagnose" | "propose";
  state: "active" | "resolved" | "stopped";
  /** @deprecated Persisted for compatibility; watches are lifetime-bound. */
  max_episodes?: number;
  delivered_episodes: number;
  cooldown_seconds: number;
  kinds?: string[];
  created_at: string;
}

// createAssistantRunWatch links a host-selected assistant run to one rooted
// run tree. A descendant request widens and returns its covering ancestor
// watch instead of opening a duplicate. The server owns delivery and only wakes a manifest-declared chat
// boundary; this is unrelated to the native-ticket addWatch API above.
export async function createAssistantRunWatch(
  targetRunId: string,
  input: {
    assistant_run_id: string;
    mode: "diagnose" | "propose";
    /** @deprecated Accepted by older servers/clients and ignored by current servers. */
    max_episodes?: number;
    cooldown_seconds?: number;
    kinds?: Array<"run.failed" | "run.finished" | "run.cancelled" | "run.stalled" | "run.paused">;
  },
): Promise<AssistantRunWatch> {
  return request(`/runs/${encodeURIComponent(targetRunId)}/assistant-watches`, {
    method: "POST",
    body: JSON.stringify(input),
  });
}

export async function listAssistantRunWatches(
  targetRunId: string,
): Promise<AssistantRunWatch[]> {
  return request(`/runs/${encodeURIComponent(targetRunId)}/assistant-watches`);
}

// RunVeille is what the assistant run is STANDING BY on, both channels at
// once. They travel together because a button that cuts only the run watches
// is not idempotent: the card subscription re-arms a fresh watch at that
// card's next dispatch, so the operator who pressed "stop" gets spoken to
// again anyway.
export interface RunVeille {
  run_watches: AssistantRunWatch[];
  watched_issue_ids: string[];
}

// listRunVeille is the reconciliation read behind the dock's live
// assistant_veille_* events: the banner is pushed by those, this is what a
// freshly mounted dock (or one that missed an event) calls to catch up.
// deliverHostEvent wakes a PARKED conversational run with a host-attested
// event, through the chat gate's host_event field. It is how the studio tells
// an assistant that something it asked for has happened — the completion
// signal without which a multi-step repair stalls after its first action,
// because parking a run is not an outcome and fires no watch episode.
//
// 409 means "not at a chat boundary": the assistant is mid-turn and the
// caller should retry, not report a failure.
export async function deliverHostEvent(
  runId: string,
  kind: string,
  event: Record<string, unknown>,
): Promise<{ delivered: boolean }> {
  return request(`/runs/${encodeURIComponent(runId)}/host-event`, {
    method: "POST",
    body: JSON.stringify({ kind, event }),
  });
}

export async function listRunVeille(runId: string): Promise<RunVeille> {
  return request(`/runs/${encodeURIComponent(runId)}/watching`);
}

export async function stopAssistantRunWatch(
  watchId: string,
): Promise<{ id: string; state: "stopped" }> {
  return request(`/assistant-watches/${encodeURIComponent(watchId)}`, {
    method: "DELETE",
  });
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

// rewindRun re-anchors an existing run and invalidates downstream engine
// state. Callers choose file restoration explicitly; assistant actions always
// send restore_scope:none so a model cannot roll back workspace files.
export async function rewindRun(
  runId: string,
  req: RewindRunRequest,
): Promise<RewindRunResponse> {
  return request(`/runs/${encodeURIComponent(runId)}/rewind`, {
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
