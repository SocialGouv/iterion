// Run-console HTTP client. Mirrors the Go service in pkg/runview/.

import { desktop, isDesktop } from "@/lib/desktopBridge";
import { apiRequest } from "./client";

const BASE_URL = import.meta.env.VITE_API_URL ?? "/api";

// Delegate to the shared apiRequest so this module picks up:
//   - credentials: "include" (needed in cross-origin cloud deployments)
//   - the 401 → onUnauthorized hook the AuthProvider registers
// Without this, every run-console call (list, get, launch, cancel,
// resume, ...) silently broke in cloud mode and failed to surface
// expired-session 401s.
function request<T>(path: string, init?: RequestInit): Promise<T> {
  return apiRequest<T>(`${BASE_URL}${path}`, init);
}

// apiURL returns the full URL for a given API path, suitable for
// hand-off to <a href> / browser download (no JSON parsing). Same
// BASE_URL resolution as `request` so dev / prod / cloud mode all
// work without callers knowing the deployment.
export function apiURL(path: string): string {
  return `${BASE_URL}${path}`;
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
  // Cloud-only: 1-based queue position when status === "queued".
  // Computed server-side; the QueuedBanner uses it to render the
  // "3rd in queue" copy. See cloud-ready plan §F (T-03, T-15, T-31).
  queue_position?: number;
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
  // Byte position in the run's log buffer at the moment this event
  // was persisted. Stamped by the backend store from the per-run log
  // buffer total. Used by the time-travel scrubber / replay to slice
  // the live log "up to where the log was when this event fired".
  // Absent on legacy events (pre-feature) and on cloud-mode events
  // where there is no on-host log buffer to attach.
  log_offset?: number;
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
  // least one node_started for this IR node id. Used by the studio's
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

// Shape of GET /api/runs/global-active — runs currently active in
// ANY iterion store on the host (the global ~/.iterion slot plus
// every per-project store under ~/.iterion/projects/). Surfaced on
// the Home view so an operator sees in-flight work without having
// to open each project first.
export interface GlobalActiveRun {
  id: string;
  name?: string;
  workflow_name: string;
  status: RunStatus;
  created_at: string;
  updated_at: string;
  store_path: string;
  workspace_dir?: string;
}

export async function listGlobalActiveRuns(): Promise<GlobalActiveRun[]> {
  const res = await request<{ runs: GlobalActiveRun[] }>(`/runs/global-active`);
  return res.runs ?? [];
}

// readStoreOverrideFromURL returns the current page's `?store=` query
// param, if any. The Home banner appends it when navigating to a run
// living in a foreign iterion store (typically the global ~/.iterion/
// slot) so this daemon's read endpoints route via the cross-store
// proxy (pkg/server/runs.go::resolveCrossStore).
//
// Empty string when not in a browser context, when no override is set,
// or when the value isn't a non-empty string.
function readStoreOverrideFromURL(): string {
  if (typeof window === "undefined") return "";
  try {
    const v = new URLSearchParams(window.location.search).get("store");
    return isSafeStoreParam(v) ? (v as string) : "";
  } catch {
    return "";
  }
}

// isSafeStoreParam validates the cross-store override path shape before
// forwarding it to the daemon. The server-side resolveCrossStore guard
// rejects unknown stores, but client-side filtering is defence-in-depth
// against a hostile `?store=...` URL handed to a victim — keep the
// charset tight and reject path-traversal segments.
// Exported so the WS hook can reuse the same predicate.
export function isSafeStoreParam(v: string | null): boolean {
  if (!v || v.length === 0 || v.length > 512) return false;
  if (!/^[A-Za-z0-9_./-]+$/.test(v)) return false;
  if (v.includes("..")) return false;
  return true;
}

// withStoreParam appends `store=<override>` to the given URLSearchParams
// when a cross-store override is active. No-op otherwise. Centralised
// here so every run-scoped API call routes consistently.
function withStoreParam(qs: URLSearchParams): URLSearchParams {
  const override = readStoreOverrideFromURL();
  if (override) qs.set("store", override);
  return qs;
}

export async function getRun(
  runId: string,
  opts?: { signal?: AbortSignal },
): Promise<RunSnapshot> {
  const qs = withStoreParam(new URLSearchParams()).toString();
  return request(
    `/runs/${encodeURIComponent(runId)}${qs ? `?${qs}` : ""}`,
    { signal: opts?.signal },
  );
}

export async function loadEvents(
  runId: string,
  from = 0,
  to = 0,
): Promise<RunEvent[]> {
  const qs = new URLSearchParams();
  if (from > 0) qs.set("from", String(from));
  if (to > 0) qs.set("to", String(to));
  withStoreParam(qs);
  const suffix = qs.toString();
  const res = await request<{ events: RunEvent[] }>(
    `/runs/${encodeURIComponent(runId)}/events${suffix ? `?${suffix}` : ""}`,
  );
  return res.events ?? [];
}

export interface ToolBlobChunk {
  data: string;
  total: number;
  eof: boolean;
}

// fetchToolBlob streams a slice of a tool's stored I/O sidecar (written
// by the backend hooks layer when an input/output exceeded the inline
// threshold). offset is the byte offset to start at; limit caps bytes
// returned (0 = "all from offset"). Returns the bytes as a UTF-8 string
// plus the full size and an eof flag so the UI can keep fetching until
// the end. Throws on network / status errors; a 404 means the call's
// payload fit inline (no sidecar) — callers should fall back to the
// preview field in that case.
export async function fetchToolBlob(
  runId: string,
  toolUseID: string,
  kind: "input" | "output",
  offset = 0,
  limit = 0,
): Promise<ToolBlobChunk> {
  const qs = new URLSearchParams();
  if (offset > 0) qs.set("offset", String(offset));
  if (limit > 0) qs.set("limit", String(limit));
  const suffix = qs.toString();
  const url = `${BASE_URL}/runs/${encodeURIComponent(runId)}/tools/${encodeURIComponent(toolUseID)}/${kind}${suffix ? `?${suffix}` : ""}`;
  const res = await fetch(url, { credentials: "include" });
  if (!res.ok) {
    throw new Error(`API error ${res.status}: ${await res.text()}`);
  }
  const data = await res.text();
  const totalHeader = res.headers.get("X-Tool-Total-Size") ?? "0";
  const total = Number.parseInt(totalHeader, 10) || data.length;
  const eofHeader = res.headers.get("X-Tool-Eof") ?? "";
  const eof = eofHeader === "true";
  return { data, total, eof };
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

export interface WireNode {
  id: string;
  kind: string;
  model?: string;
  backend?: string;
  reasoning_effort?: string;
  output_schema?: WireSchemaField[];
}

export interface WireSchemaField {
  name: string;
  // "string" | "bool" | "int" | "float" | "json" | "string[]"
  type: string;
  enum_values?: string[];
}

export async function getRunWorkflow(runId: string): Promise<WireWorkflow> {
  const qs = withStoreParam(new URLSearchParams()).toString();
  return request(`/runs/${encodeURIComponent(runId)}/workflow${qs ? `?${qs}` : ""}`);
}

export async function listArtifacts(
  runId: string,
  nodeId: string,
  opts?: { signal?: AbortSignal },
): Promise<ArtifactSummary[]> {
  const res = await request<{ artifacts: ArtifactSummary[] }>(
    `/runs/${encodeURIComponent(runId)}/artifacts/${encodeURIComponent(nodeId)}`,
    { signal: opts?.signal },
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

// ArtifactFile is one tool-produced file from the run's artifact_files
// area (renovacy reports, SBOMs, …). Distinct from `Artifact` (the
// versioned per-node JSON output) — these are arbitrary files that
// in-sandbox tools wrote via $ITERION_ARTIFACT_FILES_DIR.
export interface ArtifactFile {
  path: string; // area-relative, slash-separated
  size: number;
  modified_at: string;
}

export async function listArtifactFiles(runId: string): Promise<ArtifactFile[]> {
  const res = await request<{ files: ArtifactFile[] }>(
    `/runs/${encodeURIComponent(runId)}/artifact-files`,
  );
  return res.files ?? [];
}

// Build the URL to download a single artifact file. Returns a string
// (not a fetch wrapper) because the caller hands it straight to an
// `<a href>` for browser download / new-tab preview.
export function artifactFileURL(runId: string, relPath: string): string {
  // The path can contain `/` segments; encodeURIComponent would clobber
  // them. Encode each segment individually so subdirs survive.
  const segments = relPath.split("/").map(encodeURIComponent).join("/");
  return apiURL(`/runs/${encodeURIComponent(runId)}/artifact-files/${segments}`);
}

// fetchArtifactFile downloads one artifact file body via the same
// auth-aware fetch surface as every other API call (cookies + Bearer).
// `download=true` flips the backend's Content-Disposition to
// `attachment` so previewable content types (json, md) still trigger
// a real download instead of an inline render.
export async function fetchArtifactFile(
  runId: string,
  relPath: string,
  opts: { download?: boolean } = {},
): Promise<{ blob: Blob; contentType: string }> {
  const url = artifactFileURL(runId, relPath) + (opts.download ? "?download=1" : "");
  const res = await fetch(url, { credentials: "include" });
  if (!res.ok) {
    throw new Error(`API error ${res.status}: ${await res.text()}`);
  }
  return {
    blob: await res.blob(),
    contentType: res.headers.get("Content-Type") ?? "application/octet-stream",
  };
}

// DownloadOutcome describes what happened on the save side. `cancelled`
// is desktop-only — it fires when the user dismisses the native save
// dialog. In browser mode the SPA can't observe the user's choice
// (the download is handed off to the browser's download manager) so
// `cancelled` is always false there.
export interface DownloadOutcome {
  cancelled: boolean;
  // Absolute path of the saved file. Only populated in desktop mode
  // when the save dialog completed; undefined in browser mode (the
  // browser's downloads folder is opaque to the SPA).
  localPath?: string;
  contentType: string;
}

// downloadArtifactFile fetches the file and saves it to disk. In
// desktop mode (Wails) it routes through the SaveBinaryFile native
// binding so a real save dialog opens — the embedded WebKit silently
// swallows `<a download>` blob URLs, which is why this can't just
// rely on the DOM trick. In browser mode we fall back to the blob
// URL approach, which the user's browser handles natively.
export async function downloadArtifactFile(
  runId: string,
  relPath: string,
): Promise<DownloadOutcome> {
  const { blob, contentType } = await fetchArtifactFile(runId, relPath, { download: true });
  const basename = relPath.includes("/")
    ? relPath.slice(relPath.lastIndexOf("/") + 1)
    : relPath;

  if (isDesktop()) {
    const b64 = await blobToBase64(blob);
    const localPath = await desktop.saveBinaryFile(basename, b64);
    if (!localPath) {
      // User cancelled the native save dialog.
      return { cancelled: true, contentType };
    }
    return { cancelled: false, localPath, contentType };
  }

  const blobURL = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = blobURL;
  a.download = basename;
  a.style.display = "none";
  document.body.appendChild(a);
  a.click();
  a.remove();
  // Defer revoke so the browser has time to start the download.
  // 0ms wasn't enough on WebKit (Safari/Wails) — the a.click() handoff
  // sometimes hadn't kicked the download by the next microtask, and
  // the revoke nuked the blob, producing "blob URL not found"
  // download failures. 5s lines up with LogLinesView's pattern.
  setTimeout(() => URL.revokeObjectURL(blobURL), 5000);
  return { cancelled: false, contentType };
}

// blobToBase64 strips the `data:<mime>;base64,` prefix from FileReader
// output and hands back the raw payload — the Wails SaveBinaryFile
// binding decodes plain base64 (no data-URL wrapper).
async function blobToBase64(blob: Blob): Promise<string> {
  const buf = await blob.arrayBuffer();
  const bytes = new Uint8Array(buf);
  // Build the binary-string in 32 KB pieces and join at the end:
  // `binary += String.fromCharCode(...)` would be O(N²) on the total
  // size (every += copies an ever-growing immutable string), freezing
  // the UI thread for several seconds on multi-MB artifact downloads.
  // Array#join allocates once over the concatenated length.
  const parts: string[] = [];
  const chunk = 0x8000;
  for (let i = 0; i < bytes.length; i += chunk) {
    parts.push(String.fromCharCode(...bytes.subarray(i, i + chunk)));
  }
  return btoa(parts.join(""));
}

export interface CreateRunRequest {
  file_path: string;
  // Inline .iter source — required in cloud mode (no shared FS),
  // ignored in local mode where file_path resolves on disk.
  source?: string;
  run_id?: string;
  vars?: Record<string, string>;
  // Name of an in-source preset (presets: block) to apply before vars.
  preset?: string;
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
  // Attachments uploaded via POST /api/runs/uploads. Map of the
  // workflow's attachment name → upload_id returned by the staging
  // endpoint. The server promotes each upload into the run-scoped
  // store before the engine starts.
  attachments?: Record<string, string>;
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
  // See CreateRunRequest.source.
  source?: string;
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
  // Line counts from `git diff --numstat`, populated by the backend.
  // Sentinel: added/deleted = -1 alongside binary=true means the file
  // is binary and the FilesPanel should render "(binary)" instead of
  // "+N | -N". Otherwise both fields are real line counts; 0 is
  // meaningful for pure renames or whitespace-only diffs.
  added: number;
  deleted: number;
  binary?: boolean;
}

// File listing source-of-truth selector. Mirrors the server's fileMode:
//   - "uncommitted": worktree `git status` (changes pending commit).
//   - "branch": BaseCommit..HEAD range (commits introduced by the run).
// Empty string means "let the backend pick the default" (the live
// uncommitted view when a worktree exists, branch otherwise).
export type RunFilesMode = "uncommitted" | "branch" | "";

// Mirror of server.runFilesResponse. `available` is the gate: when
// false, `reason` is one of "no_workdir" | "not_git_repo" |
// "no_baseline" | "worktree_gone" and the studio renders an empty-
// state instead of a file list.
//
// `live` distinguishes the source: true when files come from a
// still-existing worktree (uncommitted or live branch range), false
// when from the post-finalization historical diff. `mode` reflects
// the effective view so the segmented control can highlight the
// active option without re-deriving from `live`.
export interface RunFiles {
  work_dir?: string;
  worktree?: boolean;
  live?: boolean;
  mode?: RunFilesMode;
  files: RunFile[];
  available: boolean;
  reason?:
    | "no_workdir"
    | "not_git_repo"
    | "no_baseline"
    | "worktree_gone"
    | string;
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

export async function listRunFiles(
  runId: string,
  opts: { mode?: RunFilesMode } = {},
): Promise<RunFiles> {
  const qs = new URLSearchParams();
  if (opts.mode) qs.set("mode", opts.mode);
  const suffix = qs.toString();
  return request(
    `/runs/${encodeURIComponent(runId)}/files${suffix ? `?${suffix}` : ""}`,
  );
}

export async function getRunFileDiff(
  runId: string,
  path: string,
  opts: { mode?: RunFilesMode } = {},
): Promise<RunFileDiff> {
  const qs = new URLSearchParams({ path });
  if (opts.mode) qs.set("mode", opts.mode);
  return request(
    `/runs/${encodeURIComponent(runId)}/files/diff?${qs.toString()}`,
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
  // The message the deferred-merge endpoint would commit if no override
  // is supplied. Pre-fills the Commits-tab squash editor so the user
  // sees the proposal before clicking and only types in edit mode when
  // they want to override. Empty when the merge action is unavailable.
  default_squash_message?: string;
  available: boolean;
  reason?: "no_workdir" | "no_baseline" | "not_git_repo" | string;
}

export async function listRunCommits(runId: string): Promise<RunCommits> {
  return request(`/runs/${encodeURIComponent(runId)}/commits`);
}

// Mirror of server.runCommitDetailResponse. `available` mirrors the
// listing endpoints' contract: when false, `reason` is "not_in_range"
// and the UI renders a "commit not part of this run" empty state.
export interface RunCommitDetail {
  sha: string;
  short: string;
  parent?: string;
  subject?: string;
  author?: string;
  email?: string;
  date?: string; // RFC3339
  files: RunFile[];
  available: boolean;
  reason?: "not_in_range" | string;
}

export async function getRunCommit(
  runId: string,
  sha: string,
): Promise<RunCommitDetail> {
  return request(
    `/runs/${encodeURIComponent(runId)}/commits/${encodeURIComponent(sha)}`,
  );
}

export async function getRunCommitFileDiff(
  runId: string,
  sha: string,
  path: string,
): Promise<RunFileDiff> {
  return request(
    `/runs/${encodeURIComponent(runId)}/commits/${encodeURIComponent(
      sha,
    )}/diff?path=${encodeURIComponent(path)}`,
  );
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

// ---------------------------------------------------------------------------
// Attachments — staged uploads + server info.
// ---------------------------------------------------------------------------

import type { ServerInfo, StagedUpload } from "./types";

/** GET /api/server/info — mode, version, upload limits. */
export async function getServerInfo(): Promise<ServerInfo> {
  return request("/server/info");
}

export interface UploadOptions {
  onProgress?: (loaded: number, total: number) => void;
  signal?: AbortSignal;
  declaredMime?: string;
}

/**
 * POST /api/runs/uploads — upload a single attachment to the server's
 * staging area. Uses XMLHttpRequest because fetch() in browsers does
 * not yet expose request-side upload progress (ReadableStream upload
 * is half-duplex and Chromium-only).
 */
export function uploadAttachment(
  file: File,
  opts: UploadOptions = {},
): Promise<StagedUpload> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    const fd = new FormData();
    fd.append("file", file, file.name);
    if (opts.declaredMime) fd.append("declared_mime", opts.declaredMime);

    xhr.open("POST", `${BASE_URL}/runs/uploads`, true);
    xhr.responseType = "json";

    xhr.upload.onprogress = (evt) => {
      if (opts.onProgress && evt.lengthComputable) {
        opts.onProgress(evt.loaded, evt.total);
      }
    };
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        resolve(xhr.response as StagedUpload);
      } else {
        const body = xhr.response;
        const message =
          body && typeof body === "object" && "error" in body
            ? (body as { error: string }).error
            : `HTTP ${xhr.status}`;
        reject(new Error(message));
      }
    };
    xhr.onerror = () => reject(new Error("network error"));
    xhr.onabort = () => reject(new DOMException("aborted", "AbortError"));

    if (opts.signal) {
      if (opts.signal.aborted) {
        xhr.abort();
        return;
      }
      opts.signal.addEventListener("abort", () => xhr.abort(), { once: true });
    }

    xhr.send(fd);
  });
}
