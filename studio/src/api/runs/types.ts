// Extracted from api/runs.ts to keep that file focused.
// Pure wire-shape types shared across every runs/* submodule (mirrors
// of Go structs in pkg/runview, pkg/store, pkg/git, pkg/server).

// ServerInfo/StagedUpload live in the AST `./types` module but the runs
// barrel re-exports them so existing `from "@/api/runs"` imports keep
// resolving.
export type { ServerInfo, StagedUpload } from "../types";

export type RunStatus =
  | "running"
  | "paused_waiting_human"
  // Operator-initiated soft pause via POST /api/runs/:id/pause or the
  // RunHeader Pause button. Distinct from paused_waiting_human (no
  // pending Interaction record). Resumes via the same machinery as
  // cancelled — see Engine.Resume's dispatch.
  | "paused_operator"
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

// Derived classification of how a run was triggered. Mirror of
// pkg/runview.deriveSourceKind. The backend omits the field when it
// would be the default ("manual") and pre-source_kind legacy runs
// lack it entirely — callers must treat an empty value as "manual".
export type RunSourceKind =
  | "manual"
  | "webhook"
  | "dispatcher"
  | "fork"
  | "shard";

// Mirror of runview.RunSummary.
export interface RunSummary {
  id: string;
  // Deterministic, human-friendly run label. Empty for legacy runs
  // persisted before this field existed; UI falls back to workflow_name.
  name?: string;
  workflow_name: string;
  // Bot/bundle label (e.g. "docs-refresh"). Sourced from the bundle's
  // manifest.yaml name; server falls back to basename(bundle_path) for
  // legacy runs. Empty for plain .bot runs with no bundle.
  bundle_name?: string;
  // Bot persona label (e.g. "Nexie") from the bundle manifest. Empty for
  // plain runs; render falls back to bundle_name then workflow_name.
  // Used for readable "by bot" filter chips.
  bundle_display_name?: string;
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
  // Populated when the worktree produced commits but the persistent
  // storage branch could not be created (malformed default name, git
  // failure). The commits are reachable only via reflog until the
  // operator runs `git branch <name> <final_commit>` manually.
  final_branch_error?: string;
  merged_into?: string;
  merged_commit?: string;
  merge_strategy?: MergeStrategy;
  merge_status?: MergeStatus;
  auto_merge?: boolean;
  // Local-mode run location, powering the "by folder" filter in
  // desktop/local mode: work_dir is the absolute exec dir (worktree or
  // cwd), repo_root the main git repo. Empty for cloud runs and legacy
  // runs.
  work_dir?: string;
  repo_root?: string;
  // Cloud-mode stable forge slug ("group/project") powering the "by
  // repo" filter. Empty in local mode.
  project_path?: string;
  // Cloud-only: 1-based queue position when status === "queued".
  // Computed server-side via Mongo aggregation; the UI uses it for the
  // queued banner copy ("3rd in queue"). See cloud-ready plan §F (T-03,
  // T-31).
  queue_position?: number;
  // Derived classifier (manual | webhook | dispatcher | fork | shard).
  // Server omits the field for legacy runs and for the default value
  // "manual"; the UI must treat empty as "manual" — see
  // runSourceMeta.normalizeSourceKind.
  source_kind?: RunSourceKind;
}

export type MergeStrategy = "squash" | "merge";
export type MergeStatus =
  | "pending"
  | "merged"
  | "skipped"
  | "failed"
  // `git merge --squash` produced content conflicts; the worktree is
  // currently in the conflicted state (markers on disk, UU paths in
  // the index). The studio renders MergeConflictView until the
  // operator resolves every file + finalizes or aborts.
  | "conflicted";

// Mirror of sessionboard.Widget / sessionboard.Spec (Go). The LLM
// curation layer (Phase 2) emits these; the studio renders one card per
// widget on the Tasks tab beneath the deterministic task list. `kind`
// selects the renderer; `props` is the kind-specific payload (the studio
// ignores unknown kinds for forward-compat).
export type SessionBoardWidgetKind =
  | "note"
  | "metric"
  | "checklist"
  | "progress"
  | "bar_chart";

export interface SessionBoardWidget {
  id: string;
  kind: SessionBoardWidgetKind | string;
  title?: string;
  props?: Record<string, unknown>;
}

export interface SessionBoardSpec {
  version: number;
  widgets?: SessionBoardWidget[];
  updated_seq?: number;
}

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
// One launch-time model/backend override rule captured on the run: the
// node selector (id / glob / kind / "*") plus whatever it pins. Mirrors
// pkg/store.RunModelOverride.
export interface RunModelOverride {
  selector: string;
  backend?: string;
  model?: string;
  provider?: string;
}

export interface RunHeader {
  id: string;
  // Deterministic, human-friendly run label. Empty for legacy runs
  // persisted before this field existed; UI falls back to workflow_name.
  name?: string;
  workflow_name: string;
  workflow_hash?: string;
  file_path?: string;
  // Bundle's manifest.yaml `name` field captured at launch (e.g.
  // "feature-dev"). May differ from workflow_name when the bundle
  // ships a customised manifest. Empty for plain .bot runs.
  bundle_name?: string;
  // Bundle's manifest.yaml `display_name` field — the persona an
  // operator actually uses in conversation (e.g. "Nexie"). When
  // set, the studio adds a ✨ icon next to the bot chip.
  bundle_display_name?: string;
  status: RunStatus;
  inputs?: Record<string, unknown>;
  // Workflow-declared tool-permission gate mode ("off" | "ask" | "deny").
  // Empty/off = no gate. The header badges ask/deny. See docs/permissions.md.
  permission_mode?: string;
  // Launch-time per-node/-group model/backend pins captured on the run
  // (studio dropdowns / CLI --model/--backend / HTTP model_overrides).
  // Display-only, surfaced in the Overview's "Launched with". Empty when
  // none were set.
  model_overrides?: RunModelOverride[];
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
  // Populated when the worktree produced commits but the persistent
  // storage branch could not be created (malformed default name, git
  // failure). The commits are reachable only via reflog until the
  // operator runs `git branch <name> <final_commit>` manually.
  final_branch_error?: string;
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
  // forked_from + fork_anchor are set when this run was minted by
  // POST /api/runs/:id/fork. The RunHeader surfaces them as a "forked
  // from <parent>" breadcrumb; the InnerTabBar adds a ⑂ glyph on the
  // run's tab. Both empty/undefined for runs launched normally.
  forked_from?: string;
  fork_anchor?: ForkAnchor;
  // source_hash mirrors the parent's workflow hash at fork time.
  // Different from workflow_hash (the child's own) when the workflow has
  // been edited between parent run and fork.
  source_hash?: string;
  // source describes the originating action that produced this run.
  // Today only dispatcher runs populate it, carrying the back-reference
  // to the kanban issue so the RunHeader can link back to /board.
  source?: RunSource;
  // watched_issue_ids is the server-authoritative set of native-kanban
  // issue IDs this run subscribed to (MVP3b). The whats-next WatchPanel
  // reads it as the primary watch-list source; absent for legacy runs
  // that predate the field (the UI then falls back to event derivation).
  watched_issue_ids?: string[];
  // loops is the run-level "real loops" indicator: one entry per
  // declared named loop (e.g. "review_loop"), reporting the SEMANTIC
  // iteration counter (matching the runtime's `node#N` log label), NOT
  // the count of node executions. Absent for runs with no named loops.
  loops?: Record<string, RunLoopProgress>;
}

// RunLoopProgress reports a named loop's semantic progress: the current
// iteration counter and its declared bound (max 0/absent = unbounded or
// unknown).
export interface RunLoopProgress {
  current: number;
  max?: number;
}

export interface RunSource {
  kind?: string;
  issue_id?: string;
  issue_identifier?: string;
  issue_title?: string;
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
  // Labels categorise the artifact (e.g. "plan", "verdict"). Mirror of
  // store.Artifact.Labels. Empty/absent on legacy artifacts.
  labels?: string[];
  written_at: string;
}

// RunArtifactSummary mirrors runview.RunArtifactSummary: the latest
// published artifact per node, for the centralized label-grouped view.
export interface RunArtifactSummary {
  node_id: string;
  version: number;
  labels?: string[];
  title?: string;
  written_at: string;
}

export interface ListRunsParams {
  status?: RunStatus | "";
  workflow?: string;
  // Repo filters runs to a stable forge slug (project_path) — cloud mode
  // only. Local-mode folder filtering is client-side (the server has no
  // project_path on local runs), so the studio must not send this in
  // local mode (it would match nothing).
  repo?: string;
  since?: string; // RFC3339
  limit?: number;
  // Node filters runs to those whose persisted events include at
  // least one node_started for this IR node id. Used by the studio's
  // "this node was touched by N runs" chip on hover/select.
  node?: string;
}

// One repository (project_path) that has runs, with a per-repo count.
// Mirror of server.RepoBucket. Returned by GET /api/v1/runs/repos.
export interface RunRepo {
  project_path: string;
  count: number;
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

export interface ToolBlobChunk {
  data: string;
  total: number;
  eof: boolean;
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

// ArtifactFile is one tool-produced file from the run's artifact_files
// area (renovacy reports, SBOMs, …). Distinct from `Artifact` (the
// versioned per-node JSON output) — these are arbitrary files that
// in-sandbox tools wrote via $ITERION_ARTIFACT_FILES_DIR.
export interface ArtifactFile {
  path: string; // area-relative, slash-separated
  size: number;
  modified_at: string;
}

// PlanTodo is one entry in a persisted plan snapshot — the normalized
// TodoWrite/todo_write item shape emitted by the Go store. Status is
// canonicalised server-side to the claude_code vocabulary
// (pending | in_progress | completed).
export interface PlanTodo {
  content: string;
  status: string;
  active_form?: string;
  priority?: string;
  id?: string;
}

// PlanSnapshot is one chronological snapshot of an agent's living TODO
// plan (captured when a TodoWrite/todo_write tool fired), served by
// GET /api/runs/:id/plans in ascending seq order.
export interface PlanSnapshot {
  seq: number;
  node_id: string;
  iteration: number;
  tool?: string;
  ts: string;
  todos: PlanTodo[];
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

export interface CreateRunRequest {
  file_path: string;
  // Inline workflow source — required in cloud mode (no shared FS),
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
  // Backend, when set, overrides the workflow's `default_backend:`
  // for this run only. Node-level explicit `backend:` still wins.
  // Empty preserves the resolver chain (workflow default → env →
  // auto-detect). Useful for A/B-testing the same workflow against
  // different backends without editing the workflow source.
  backend?: string;
  // command-output-compression override ("on" | "ultra" | "off").
  // Empty inherits the workflow/node `compress:` DSL then ITERION_COMPRESS.
  // Rewrites agent shell commands via the active rewriter plugin chain.
  compress?: string;
  // tool-permission gate mode ("off" | "ask" | "deny"). Empty inherits
  // the workflow/node `permission:` DSL then ITERION_PERMISSION. "ask"
  // pauses for human approval on any tool not allow-listed; "deny" hard-
  // blocks it. See docs/permissions.md.
  permission?: string;
  // Per-node/-group model+backend overrides (Launch dropdowns). Each entry
  // targets nodes by selector (node id, id glob "reviewer_*", or kind
  // keyword "agent"|"judge") and wins over the node's DSL backend:/model:.
  // Composes with review_mode. See pkg/backend/model.ModelOverrides.
  model_overrides?: ModelOverrideEntry[];
}

// ModelOverrideEntry is one Launch-time per-node/-group model+backend
// override. Empty backend/model/provider leave that dimension unchanged.
export interface ModelOverrideEntry {
  selector: string;
  backend?: string;
  model?: string;
  provider?: string;
}

export interface CreateRunResponse {
  run_id: string;
  status: string;
}

// Inline cost-estimate shown next to the Launch button. Best-effort
// hint — see pkg/backend/cost for pricing caveats. Empty `nodes` and
// notes containing `no_llm_nodes` / `no_pricing_data` / `workflow_unparseable`
// signal that the chip should be hidden rather than blocking the form.
export interface PreviewCostNode {
  node_id: string;
  kind: "agent" | "judge";
  model?: string;
  effort?: string;
  tokens_in: number;
  tokens_out: number;
  cost_min_usd: number;
  cost_max_usd: number;
}

export interface PreviewCostResponse {
  tokens_min: number;
  tokens_max: number;
  cost_min_usd: number;
  cost_max_usd: number;
  nodes: PreviewCostNode[];
  notes?: string[];
}

// ForkAnchor identifies where a forked run resumes inside the parent's
// execution graph. Mirrors the Go store.ForkAnchor on the wire.
export interface ForkAnchor {
  node_id: string;
  loop_iter: number;
  turn_index: number;
  rewind_code?: boolean;
}

// ForkRunRequest is the body of POST /api/runs/:id/fork. node_id is
// required; turn_index defaults to -1 (latest turn for the node).
// rewind_code=false (default) inherits the parent's current files;
// rewind_code=true resets the new worktree to the per-node snapshot
// captured at the chosen boundary (Phase 2+).
export interface ForkRunRequest {
  node_id: string;
  turn_index?: number;
  rewind_code?: boolean;
  fork_name?: string;
  new_inputs?: Record<string, unknown>;
}

// ForkRunResponse is the JSON body returned by POST /runs/:id/fork.
export interface ForkRunResponse {
  new_run_id: string;
  parent_run_id: string;
  fork_anchor?: ForkAnchor;
}

export interface ResumeRunRequest {
  file_path?: string;
  // See CreateRunRequest.source.
  source?: string;
  answers?: Record<string, unknown>;
  force?: boolean;
  timeout?: string;
}

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
  // Populated ONLY in the "combined" view (server merges committed +
  // uncommitted): "committed" = the change landed on the run's branch,
  // "uncommitted" = still pending in the working tree. Absent in every
  // other mode. Drives the FilesPanel's subtle per-file tint and the
  // per-row diff mode (committed → branch range, uncommitted → worktree).
  lifecycle?: "committed" | "uncommitted";
}

// File listing source-of-truth selector. Mirrors the server's fileMode:
//   - "uncommitted": worktree `git status` (changes pending commit).
//   - "branch": BaseCommit..HEAD range (commits introduced by the run).
//   - "combined": union of branch + uncommitted, each file tagged with a
//     `lifecycle`. The studio's default while a run is in progress.
// Empty string means "let the backend pick the default" (the live
// uncommitted view when a worktree exists, branch otherwise).
export type RunFilesMode = "uncommitted" | "branch" | "combined" | "";

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

// Mirror of server.runFileContentResponse. Raw file contents from the run's
// LIVE worktree, ready to seed an editable Monaco buffer.
//   - `exists` false → the path is not on disk yet; the editor opens a fresh
//     empty buffer (e.g. creating a `.gitignore` that doesn't exist).
//   - `binary` true → `content` is empty and the editor refuses to edit.
export interface RunFileContent {
  path: string;
  content: string;
  binary: boolean;
  exists: boolean;
}

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

export interface CommitAndFinalizeRequest {
  commit_message: string;
}

export interface CommitAndFinalizeResponse {
  run_id: string;
  final_commit: string;
  final_branch: string;
  merge_status: MergeStatus;
  merged_into?: string;
  merged_commit?: string;
  merge_strategy?: MergeStrategy;
}

// One `<<<<<<< … ======= … >>>>>>>` region inside a conflicted file.
// Line numbers are 1-indexed and refer to the current on-disk content.
export interface MergeConflictHunk {
  start_line: number;
  end_line: number;
  ours_label?: string;
  theirs_label?: string;
  ours_lines: string[];
  // Only populated when the conflict was rendered with
  // merge.conflictStyle=diff3.
  base_lines?: string[];
  theirs_lines: string[];
  context_before?: string[];
  context_after?: string[];
}

export interface MergeConflictFile {
  path: string;
  content: string;
  hunks: MergeConflictHunk[];
  // Surfaced when the file couldn't be read (e.g. deleted on one
  // side). The UI still renders the row so the operator knows it
  // needs attention.
  read_err?: string;
}

export interface MergeConflictsResponse {
  files: MergeConflictFile[];
  // Squash commit message captured at conflict-time. The finalize
  // form pre-fills with this so the operator can land the merge
  // without retyping a message.
  pending_message?: string;
  pending_merge_into?: string;
}

export interface ResolveMergeConflictRequest {
  path: string;
  content: string;
}

export interface ResolveWithAgentRequest {
  // claw model spec like "anthropic/claude-opus-4-7" or
  // "openai/gpt-5.5"; empty uses the bot's pinned default.
  model?: string;
}

export interface FinalizeMergeConflictRequest {
  message?: string;
}

export interface UploadOptions {
  onProgress?: (loaded: number, total: number) => void;
  signal?: AbortSignal;
  declaredMime?: string;
}
