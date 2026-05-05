// Run-console HTTP client. Mirrors the Go service in pkg/runview/.

const BASE_URL = import.meta.env.VITE_API_URL ?? "/api";

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`${BASE_URL}${path}`, {
    headers: { "Content-Type": "application/json" },
    ...init,
  });
  if (!res.ok) {
    throw new Error(`API error ${res.status}: ${await res.text()}`);
  }
  return res.json() as Promise<T>;
}

export type RunStatus =
  | "running"
  | "paused_waiting_human"
  | "finished"
  | "failed"
  | "failed_resumable"
  | "cancelled"
  // Cloud-mode only: run accepted by the server, sitting on the NATS
  // queue, awaiting a runner pod. Local mode never reaches this state
  // — it transitions straight to "running" in-process. See cloud-ready
  // plan §A and §F (T-03, T-31).
  | "queued";

export type ExecStatus =
  | "running"
  | "finished"
  | "failed"
  | "paused_waiting_human"
  | "skipped";

// Mirror of runview.RunSummary.
export interface RunSummary {
  id: string;
  // Deterministic, human-friendly run label. Empty for legacy runs
  // persisted before this field existed; UI falls back to workflow_name.
  name?: string;
  workflow_name: string;
  status: RunStatus;
  file_path?: string;
  created_at: string;
  updated_at: string;
  finished_at?: string;
  error?: string;
  active: boolean;
  // Worktree finalization summary; empty for non-worktree runs or
  // runs that never reached a clean exit.
  final_commit?: string;
  final_branch?: string;
  merged_into?: string;
  merged_commit?: string;
  merge_strategy?: MergeStrategy;
  merge_status?: MergeStatus;
  auto_merge?: boolean;
  // Cloud-only: 1-based queue position when status === "queued".
  // Computed server-side via Mongo aggregation; the UI uses it for the
  // queued banner copy ("3rd in queue"). See cloud-ready plan §F (T-03,
  // T-31).
  queue_position?: number;
}

export type MergeStrategy = "squash" | "merge";
export type MergeStatus = "pending" | "merged" | "skipped" | "failed";

// Mirror of runview.ExecutionState.
export interface ExecutionState {
  execution_id: string;
  ir_node_id: string;
  branch_id: string;
  loop_iteration: number;
  status: ExecStatus;
  kind?: string;
  started_at?: string;
  finished_at?: string;
  last_artifact_version?: number;
  current_event_seq: number;
  error?: string;
  first_seq: number;
  last_seq: number;
}

// Mirror of runview.RunHeader.
export interface RunHeader {
  id: string;
  // Deterministic, human-friendly run label. Empty for legacy runs
  // persisted before this field existed; UI falls back to workflow_name.
  name?: string;
  workflow_name: string;
  workflow_hash?: string;
  file_path?: string;
  status: RunStatus;
  inputs?: Record<string, unknown>;
  created_at: string;
  updated_at: string;
  finished_at?: string;
  error?: string;
  // Checkpoint shape varies; opaque is fine for the UI.
  checkpoint?: unknown;
  // Filesystem path the run executed in (worktree or cwd). Empty for
  // pre-feature runs; the modified-files panel keys off this to decide
  // whether to render at all.
  work_dir?: string;
  worktree?: boolean;
  // Worktree finalization summary; empty for non-worktree runs or
  // runs that never reached a clean exit.
  final_commit?: string;
  final_branch?: string;
  merged_into?: string;
  merged_commit?: string;
  merge_strategy?: MergeStrategy;
  merge_status?: MergeStatus;
  auto_merge?: boolean;
  // Wall-clock the run actually consumed: sum of run_started/resumed
  // → paused/failed/cancelled/interrupted/finished windows. Excludes
  // pause and failed_resumable gaps. Reducer-derived from events.
  active_duration_ms: number;
  // RFC3339 anchor of the currently-accruing window. Present only
  // while the run is actively executing; absent once it pauses,
  // fails, is cancelled, is interrupted, or finishes. The UI adds
  // (now - current_run_start) to active_duration_ms for the live
  // ticker and freezes the value once this clears.
  current_run_start?: string;
}

// Mirror of runview.RunSnapshot.
export interface RunSnapshot {
  run: RunHeader;
  executions: ExecutionState[];
  last_seq: number; // -1 sentinel when no events have been applied
}

// Mirror of store.Event (subset — the runtime emits more types than the
// reducer cares about; the rest pass through opaque).
export interface RunEvent {
  seq: number;
  timestamp: string;
  type: string;
  run_id: string;
  branch_id?: string;
  node_id?: string;
  data?: Record<string, unknown>;
}

export interface ArtifactSummary {
  version: number;
  written_at: string;
}

export interface Artifact {
  run_id: string;
  node_id: string;
  version: number;
  data: Record<string, unknown>;
  written_at: string;
}

export interface ListRunsParams {
  status?: RunStatus | "";
  workflow?: string;
  since?: string; // RFC3339
  limit?: number;
  // Node filters runs to those whose persisted events include at
  // least one node_started for this IR node id. Used by the editor's
  // "this node was touched by N runs" chip on hover/select.
  node?: string;
}

export async function listRuns(params: ListRunsParams = {}): Promise<RunSummary[]> {
  const qs = new URLSearchParams();
  if (params.status) qs.set("status", params.status);
  if (params.workflow) qs.set("workflow", params.workflow);
  if (params.since) qs.set("since", params.since);
  if (params.limit) qs.set("limit", String(params.limit));
  if (params.node) qs.set("node", params.node);
  const suffix = qs.toString();
  const res = await request<{ runs: RunSummary[] }>(
    `/runs${suffix ? `?${suffix}` : ""}`,
  );
  return res.runs ?? [];
}

export async function getRun(runId: string): Promise<RunSnapshot> {
  return request(`/runs/${encodeURIComponent(runId)}`);
}

export async function loadEvents(
  runId: string,
  from = 0,
  to = 0,
): Promise<RunEvent[]> {
  const qs = new URLSearchParams();
  if (from > 0) qs.set("from", String(from));
  if (to > 0) qs.set("to", String(to));
  const suffix = qs.toString();
  const res = await request<{ events: RunEvent[] }>(
    `/runs/${encodeURIComponent(runId)}/events${suffix ? `?${suffix}` : ""}`,
  );
  return res.events ?? [];
}

// Mirror of runview.WireWorkflow — minimal IR projection for the
// "IR overlay" view. Heavier fields (schemas, prompts, vars, full
// expression ASTs) intentionally omitted.
export interface WireWorkflow {
  name: string;
  entry: string;
  nodes: WireNode[];
  edges: Array<{
    from: string;
    to: string;
    condition?: string;
    negated?: boolean;
    expression?: string;
    loop?: string;
  }>;
  stale_hash?: boolean;
}

// Mirror of runview.WireNode. Model/backend/reasoning_effort are only
// populated for LLM-driving nodes (Agent, Judge, Router-LLM).
export interface WireNode {
  id: string;
  kind: string;
  model?: string;
  backend?: string;
  reasoning_effort?: string;
}

export async function getRunWorkflow(runId: string): Promise<WireWorkflow> {
  return request(`/runs/${encodeURIComponent(runId)}/workflow`);
}

export async function listArtifacts(
  runId: string,
  nodeId: string,
): Promise<ArtifactSummary[]> {
  const res = await request<{ artifacts: ArtifactSummary[] }>(
    `/runs/${encodeURIComponent(runId)}/artifacts/${encodeURIComponent(nodeId)}`,
  );
  return res.artifacts ?? [];
}

export async function getArtifact(
  runId: string,
  nodeId: string,
  version: number,
): Promise<Artifact> {
  return request(
    `/runs/${encodeURIComponent(runId)}/artifacts/${encodeURIComponent(nodeId)}/${version}`,
  );
}

export interface CreateRunRequest {
  file_path: string;
  run_id?: string;
  vars?: Record<string, string>;
  timeout?: string;
  // For `worktree: auto` workflows: the branch the engine will merge
  // into after the run. "" or "current" → current branch (default);
  // "none" → skip merge; <branch> → that named branch (only honoured
  // when it matches the currently-checked-out branch).
  merge_into?: string;
  // For `worktree: auto` workflows: override the storage branch
  // name (default `iterion/run/<friendly>`). Useful for landing
  // every run on a stable name (e.g. `feat/auto-fixes`).
  branch_name?: string;
  // For `worktree: auto` workflows: how to land the run's commits
  // when auto_merge is on. "squash" (default) collapses commits
  // into one; "merge" fast-forwards (preserves history).
  merge_strategy?: MergeStrategy;
  // For `worktree: auto` workflows: when true, the engine performs
  // the merge at end of run (GitLab-style "auto-merge"); when
  // false (default), the merge is deferred to a UI action.
  auto_merge?: boolean;
}

export interface CreateRunResponse {
  run_id: string;
  status: string;
}

export async function createRun(req: CreateRunRequest): Promise<CreateRunResponse> {
  return request("/runs", {
    method: "POST",
    body: JSON.stringify(req),
  });
}

export async function cancelRun(
  runId: string,
): Promise<{ run_id: string; status: string }> {
  return request(`/runs/${encodeURIComponent(runId)}/cancel`, { method: "POST" });
}

export interface ResumeRunRequest {
  file_path?: string;
  answers?: Record<string, unknown>;
  force?: boolean;
  timeout?: string;
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

// ---------------------------------------------------------------------------
// Modified-files panel — git status + diff for the run's working dir.
// ---------------------------------------------------------------------------

// Status code mirrored from pkg/git.FileStatus. "??" is git's untracked
// marker; we keep it verbatim so the UI can pattern-match without any
// translation layer.
export type RunFileStatus = "M" | "A" | "D" | "R" | "??" | string;

export interface RunFile {
  path: string;
  status: RunFileStatus;
  old_path?: string;
}

// Mirror of server.runFilesResponse. `available` is the gate: when
// false, `reason` is one of "no_workdir" | "not_git_repo" and the
// editor renders an empty-state instead of a file list.
export interface RunFiles {
  work_dir?: string;
  worktree?: boolean;
  files: RunFile[];
  available: boolean;
  reason?: "no_workdir" | "not_git_repo" | string;
}

// Mirror of pkg/git.DiffPayload. before/after are nil for added/deleted
// files respectively; binary suppresses both contents so the UI can
// substitute a "binary file" placeholder. Status is not part of the
// payload — the caller passes it through from the prior /files listing.
export interface RunFileDiff {
  path: string;
  before: string | null;
  after: string | null;
  binary: boolean;
}

export async function listRunFiles(runId: string): Promise<RunFiles> {
  return request(`/runs/${encodeURIComponent(runId)}/files`);
}

export async function getRunFileDiff(
  runId: string,
  path: string,
): Promise<RunFileDiff> {
  return request(
    `/runs/${encodeURIComponent(runId)}/files/diff?path=${encodeURIComponent(path)}`,
  );
}

// ---------------------------------------------------------------------------
// Commits panel — git log between BaseCommit and FinalCommit/HEAD.
// ---------------------------------------------------------------------------

// Mirror of pkg/git.CommitInfo. The frontend formats `date` relatively
// and shows `subject` + `short` SHA.
export interface RunCommit {
  sha: string;
  short: string;
  subject: string;
  author: string;
  email?: string;
  date: string; // RFC3339
}

// Mirror of server.runCommitsResponse.
export interface RunCommits {
  commits: RunCommit[];
  count: number;
  base_commit?: string;
  head_commit?: string;
  available: boolean;
  reason?: "no_workdir" | "no_baseline" | "not_git_repo" | string;
}

export async function listRunCommits(runId: string): Promise<RunCommits> {
  return request(`/runs/${encodeURIComponent(runId)}/commits`);
}

export interface MergeRunRequest {
  merge_strategy?: MergeStrategy;
  merge_into?: string;
  commit_message?: string;
}

export interface MergeRunResponse {
  run_id: string;
  merged_commit: string;
  merged_into: string;
  merge_strategy: MergeStrategy;
  merge_status: MergeStatus;
}

export async function mergeRun(
  runId: string,
  req: MergeRunRequest,
): Promise<MergeRunResponse> {
  return request(`/runs/${encodeURIComponent(runId)}/merge`, {
    method: "POST",
    body: JSON.stringify(req),
  });
}
