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
  | "schedule"
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
  // Machine-readable classification of `error` (ADR-095); absent = unknown.
  failure_code?: string;
  active: boolean;
  // When a failed_resumable run will be resumed automatically, once the
  // provider quota window that killed it reopens. Absent when no retry is
  // armed — which is the distinction worth rendering: a run that resumes
  // in 33h and one that never will are both "failed_resumable" otherwise.
  retry_after?: string;
  retry_attempts?: number;
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
  // Run-tree shard tuple (T4b, refs #125): the child←parent edge plus
  // the shard coordinates mirrored from the queue message. parent_run_id
  // points at the run that spawned this shard/child; the shard_* fields
  // describe its slot in the parent's fan-out. All absent for a
  // top-level (non-sharded) run. Lets the run list / children endpoint
  // project a run's shard/child subtree client-side.
  parent_run_id?: string;
  // IR node id of the subbot node in the parent workflow that spawned
  // this child run (contract C3). Absent for shard children spawned by
  // fan-out routers, plain forks, and legacy runs — the UI falls back
  // to attributing such children to the parent's single subbot node
  // when unambiguous (see lib/subRuns.groupChildrenByNode).
  parent_node_id?: string;
  shard_index?: number;
  shard_count?: number;
  shard_label?: string;
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
  | "conflicted"
  // A worker holds the merge claim (ClaimMerge CAS) and is squashing/
  // pushing right now; a second merge request is refused server-side
  // until the claim resolves or goes stale.
  | "merging";

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

// Last (backend, model) that served one LLM node. Mirror of
// pkg/store.NodeServed. `model` is what the provider reported;
// `declared_model` is what the node asked for.
export interface NodeServed {
  backend: string;
  model?: string;
  declared_model?: string;
  context_window?: number;
  max_output_tokens?: number;
}

// Effective budget cap set captured at launch — the workflow's `budget:`
// block after recipe/preset/CLI overrides and, in cloud, the platform
// ceiling clamp. Mirrors pkg/store.RunBudget. A zero/absent field means
// "no cap on that dimension"; the whole object is absent when the
// workflow declared no budget. `max_duration` is a Go duration string
// ("30m", "1h30m") — parse with parseGoDuration from lib/duration.
export interface RunBudget {
  max_cost_usd?: number;
  max_tokens?: number;
  max_iterations?: number;
  max_duration?: string;
  max_parallel_branches?: number;
}

// The persisted budget-consumption fields carried on the run checkpoint —
// authoritative across resume segments, unlike the event-derived live
// totals which reset per segment. Mirror of the store.Checkpoint budget_*
// fields; the rest of the checkpoint stays opaque.
export interface CheckpointBudget {
  budget_tokens_used?: number;
  budget_cost_usd?: number;
  budget_iterations_used?: number;
  budget_elapsed_ns?: number;
  cost_usd_total?: number;
}

// Run checkpoint as far as the UI reads it: typed budget-consumption +
// paused-node fields, remainder left opaque via the index signature.
export type RunCheckpoint = CheckpointBudget & {
  node_id?: string;
  interaction_id?: string;
  parallel?: {
    pending_node_id?: string;
  };
  [key: string]: unknown;
};

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
  // Last (backend, model) that served each LLM node, copied from
  // run.json so a snapshot is self-describing without replaying
  // events. Distinct from backends_used (event-derived unique pairs).
  nodes_served?: Record<string, NodeServed>;
  // Effective budget caps captured at launch, surfaced so the Overview
  // draws budget meters with a denominator. Absent when the workflow
  // declared no budget: block; meters then degrade to bare stats.
  budget?: RunBudget;
  created_at: string;
  updated_at: string;
  finished_at?: string;
  error?: string;
  // Machine-readable classification of `error` (ADR-095); absent = unknown.
  failure_code?: string;
  // Typed for the budget-consumption + paused-node fields the UI reads;
  // the rest of the checkpoint stays opaque. See RunCheckpoint.
  checkpoint?: RunCheckpoint;
  // Filesystem path the run executed in (worktree or cwd). Empty for
  // pre-feature runs; the modified-files panel keys off this to decide
  // whether to render at all.
  work_dir?: string;
  // Stable forge slug ("group/project") the run targets — the cloud
  // run's repo identity (work_dir is a runner-pod path there). Empty
  // for local and repo-less runs.
  project_path?: string;
  worktree?: boolean;
  // True when work_dir still exists on the server's filesystem — i.e. the
  // inline file editor + live diff surfaces can be served without a 409.
  // False for a cloud run (worktree on the runner pod) and a finalized/
  // gc'd local run (worktree torn down). The RunView gates its Monaco
  // file-editor affordances on this so an Edit click never 409s. Absent
  // (undefined) on pre-feature snapshots — treated as unavailable.
  worktree_available?: boolean;
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
  // Lines changed by the run's commits: three-dot numstat against the
  // fork point, computed+cached server-side. Absent (not zero) when the
  // refs are unresolvable — render "—", never a guessed 0.
  loc_added?: number;
  loc_deleted?: number;
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
  // source describes the originating action that produced this run:
  // a dispatcher claim (carrying the back-reference to the kanban issue
  // so the RunHeader can link back to /board) or a schedule tick
  // (carrying the schedule identity). Absent for operator launches.
  source?: RunSource;
  // Run-tree shard tuple (T4b, refs #125): parent_run_id points UP at the
  // run that spawned this shard/child; shard_index/shard_count/shard_label
  // describe this run's slot in its parent's fan-out (mirrored from the
  // queue message). All absent for a top-level (non-sharded) run. The
  // tree/shard-grid UI that renders these is a separate follow-up.
  parent_run_id?: string;
  // IR node id of the parent's subbot node that spawned this child run
  // (contract C3). See the RunSummary field of the same name.
  parent_node_id?: string;
  shard_index?: number;
  shard_count?: number;
  shard_label?: string;
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
  // backends_used summarizes the distinct (backend, model) pairs the
  // run's LLM/delegate nodes executed against, reducer-derived from each
  // node_finished's stamped _backend / _model. Auto-detected backends
  // report their resolved value, never "auto". Absent for tool/compute-
  // only runs (the header then renders no backend chip).
  backends_used?: BackendUsage[];
  // fallbacks_used lists the nodes a fallback route served instead of
  // their first choice (ADR-087). Empty on every clean run, so its
  // presence always means something happened.
  fallbacks_used?: FallbackUsage[];
  // deployment is the run's delivery outcome — live URL, running image,
  // source commit, and the traceability verdict — reducer-derived from
  // the deployment-report output contract (any node emitting the
  // reserved keys; no bot name involved). Absent for runs that deployed
  // nothing, which is nearly all of them: the header then renders no
  // deployment row at all. Mirror of runview.DeploymentReport.
  deployment?: DeploymentReport;
}

// DeploymentReport is a run's delivery outcome. deployed/healthy are the
// deploying node's own claims; trace carries the INDEPENDENT verdict on
// whether that claim is traceable back to the repository. The UI must
// never render the first without the second: a URL that answers 200
// while nothing was pushed and nothing is reproducible is not a
// delivery. Mirror of runview.DeploymentReport.
export interface DeploymentReport {
  // IR node that reported the delivery.
  node_id?: string;
  deployed: boolean;
  healthy: boolean;
  // Public address a human opens. Absent on a failed or blocked deploy.
  url?: string;
  // Exact image reference running, e.g. "ghcr.io/owner/repo:<sha>".
  image_ref?: string;
  // Source commit the delivery is anchored to. Absent when the reporter
  // did not state it — the row then shows no commit rather than
  // borrowing final_commit, which can be LATER than what was deployed.
  commit?: string;
  // The reporter's own prose: what was published, or the blocking error.
  notes?: string;
  // Traceability verdict. Absent when no traceability gate ran at all —
  // a distinct fact from a gate that ran and could not establish the
  // facts (trace.verifiable === false).
  trace?: DeploymentTrace;
}

// DeploymentTrace is the verdict on whether a delivery traces back to
// reviewable source. verifiable is the meta-fact and gates the other
// three: false means the gate could not establish them at all (git
// unreachable, gate miswired). That is NOT a failed delivery and must
// never be rendered as one. Mirror of runview.DeploymentTrace.
export interface DeploymentTrace {
  node_id?: string;
  verifiable: boolean;
  // The deployed commits are reachable from a remote branch.
  pushed: boolean;
  // The running image is published under this repo's own registry path.
  image_from_repo: boolean;
  // The image reference names the pushed commit.
  built_from_head: boolean;
  // The gate's own explanation — the remedy on a failure, the
  // environment fault when unverifiable.
  log?: string;
}

// BackendUsage is one distinct (backend, model) pair the run's LLM /
// delegate nodes executed against. node_count is the number of distinct
// IR nodes that resolved to it (loops / resumes don't inflate it); model
// is empty when the backend reported no effective model. Mirror of
// runview.BackendUsage.
export interface BackendUsage {
  backend: string;
  model?: string;
  node_count: number;
}

// FallbackUsage names one node that a fallback route served after its
// primary failed.
export interface FallbackUsage {
  node_id: string;
  // served_by is the route's declared name (a `fallbacks:` entry name,
  // or "run-fallback" for the operator's launch-time route).
  served_by?: string;
  // backend / model are what actually ran, not what was requested.
  backend?: string;
  model?: string;
  // skipped marks an `action: skip` terminal route: NOTHING served the
  // node — it completed with a zero-value output (backend/model empty).
  skipped?: boolean;
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
  // Schedule provenance (kind === "schedule"): the stable schedule
  // identity the overlap gate queries, plus its human label when it
  // differs. Mirror of store.RunSource.
  schedule_id?: string;
  schedule_name?: string;
}

// Mirror of runview.RunSnapshot.
export interface RunSnapshot {
  run: RunHeader;
  executions: ExecutionState[];
  last_seq: number; // -1 sentinel when no events have been applied
}

// RunEvent (mirror of store.Event) lives in ./events.ts as a
// discriminated union over the event `type`; re-exported here so both
// the barrel and direct "@/api/runs/types" importers keep resolving it.
export * from "./events";

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
  // Bot filters runs to a bundle name (case-insensitive server-side,
  // matches RunSummary.bundle_name). Powers the bot home's "recent
  // runs" card. Wire name: ?bot=.
  bot?: string;
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
  parent_run_id?: string;
  workflow_name: string;
  bundle_name?: string;
  bundle_display_name?: string;
  input_path?: string;
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
  // Optional authored `description:` from the .bot node — the
  // human-readable label. Falls back to humanizeNodeId(id) when absent.
  description?: string;
  model?: string;
  backend?: string;
  reasoning_effort?: string;
  // Human-node only: the gate's declared `input_schema`, i.e. the TYPE
  // of the payload it receives (the values themselves ride the pause's
  // `questions` map). Drives how the inbound payload is rendered above
  // the answer form — `json` as structured data, `file` as a preview.
  // Absent when the node declares no input schema; the renderer then
  // infers from each value's shape.
  input_schema?: WireSchemaField[];
  // Human-node only: the inbound keys the node's `instructions:` prompt
  // already interpolates (`{{input.<key>}}`). Skipped when rendering the
  // inbound payload so a gate that embeds its input in its instructions
  // — the historical workaround — shows it once, not twice.
  instruction_inputs?: string[];
  output_schema?: WireSchemaField[];
  // Subbot-only (contract C2): the child .bot file this node runs, and
  // whether the child executes in an isolated workspace. Both absent
  // for every other node kind.
  source?: string;
  isolated?: boolean;
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

// RunNote is one freeform operator note attached to a run — the durable
// annotations a team leaves ("flaky, re-ran", "root cause was X"). Served
// by GET /api/runs/:id/notes in ascending seq (chronological) order;
// created by POST /api/runs/:id/notes. Immutable once created.
export interface RunNote {
  seq: number;
  author: string;
  body: string;
  ts: string;
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
  // Catalog bundle id (e.g. "whats-next"). In cloud mode the server
  // resolves the bot's source + skills off the pod's own bots/ tree, so a
  // catalog bot launches without uploading its bytes. A catalog-shaped
  // file_path is inferred to the same id when this is omitted.
  bot_id?: string;
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
  // auto-memory (MEMORY.md) override ("on" | "off"). Empty inherits the
  // workflow/node `auto_memory:` DSL then ITERION_AUTO_MEMORY (default off).
  auto_memory?: string;
  // tool-permission gate mode ("off" | "ask" | "deny"). Empty inherits
  // the workflow/node `permission:` DSL then ITERION_PERMISSION. "ask"
  // pauses for human approval on any tool not allow-listed; "deny" hard-
  // blocks it. See docs/permissions.md.
  permission?: string;
  // Run-level mono/dual review-topology override ("auto" | "mono" |
  // "dual") for bots that declare a `review_mode` var. Omitted/"auto"
  // resolves from the providers detected at launch. See
  // pkg/reviewtopology.
  review_mode?: string;
  // Run-level budget-cap overrides — the HTTP twin of the CLI --max-*
  // flags. Non-zero fields win over the workflow's `budget:` block;
  // zero/omitted fields inherit. max_duration is a Go duration string
  // ("2h", "1h30m"), validated server-side (bad value → 400). Omit the
  // whole object when nothing is overridden.
  budget?: RunBudget;
  // Per-node/-group model+backend overrides (Launch dropdowns). Each entry
  // targets nodes by selector (node id, id glob "reviewer_*", or kind
  // keyword "agent"|"judge") and wins over the node's DSL backend:/model:.
  // Composes with review_mode. See pkg/backend/model.ModelOverrides.
  model_overrides?: ModelOverrideEntry[];
  // Run-level fallback route (ADR-087): one alternative taken when an
  // agent node's primary fails. Applies only to agent nodes that
  // declare no `fallbacks:` of their own, and never to judges.
  fallback?: FallbackEntry;
}

// FallbackEntry is the operator's single run-level fallback route.
export interface FallbackEntry {
  backend?: string;
  model?: string;
  provider?: string;
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
  effective?: PreviewEffectiveSettings;
}

// PreviewEffectiveSettings reports each launch knob's resolution BELOW
// the run-override level (workflow/env/default + node-pinned flag) so
// the Launch dialog can caption its selects with why a knob is what it
// is. The client layers its own override on top.
export interface PreviewEffectiveKnob {
  effective: string;
  source: "workflow" | "env" | "default";
  node_pinned?: boolean;
}

export interface PreviewEffectiveSettings {
  compress: PreviewEffectiveKnob;
  auto_memory: PreviewEffectiveKnob;
  permission: PreviewEffectiveKnob;
  backend: PreviewEffectiveKnob;
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
  // Ad-hoc upload IDs (from uploadAttachment) the operator attached to
  // this answer without the workflow declaring a `file` field — the
  // "here's a diagram explaining my feedback" case. The server promotes
  // them to run attachments and hands the workflow descriptors on the
  // reserved `_attachments` answer key.
  //
  // A DECLARED `file` field travels differently: its upload rides
  // inline in `answers` as `{ upload_id: "..." }` (see
  // UPLOAD_ENVELOPE_KEY), because it has a schema field name to land on.
  attachments?: string[];
}

// UPLOAD_ENVELOPE_KEY marks an answer value as "bytes I already staged"
// rather than literal JSON data. The server recognises the same shape
// and swaps it for a descriptor before the engine sees the answers.
// Keep in sync with pkg/server/runs_answer_uploads.go.
export const UPLOAD_ENVELOPE_KEY = "upload_id";

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
  // True when the producer could not compute line counts at all, so
  // added/deleted are UNPOPULATED rather than a genuine +0/-0. iterion's
  // own workspace versioning is such a producer — it stores content, not
  // diffs, and has no git to ask — so every modified text file on an
  // in-place run would otherwise render "+0 −0", reading as "nothing
  // changed in this file". Render nothing instead.
  counts_unknown?: boolean;
  // True when the path is in the range but its CONTENT was never stored
  // (over the workspace-versioning size cap), so no diff can be rendered.
  // Distinct from `binary`, where content exists but is not text. Listing
  // it is the point: the file most likely to exceed the cap is the media
  // export the run exists to produce, and this is an approval surface.
  uncaptured?: boolean;
}

// File listing source-of-truth selector. Mirrors the server's fileMode:
//   - "uncommitted": worktree `git status` (changes pending commit).
//   - "branch": BaseCommit..HEAD range (commits introduced by the run).
//   - "combined": union of branch + uncommitted, each file tagged with a
//     `lifecycle`. The studio's default while a run is in progress.
//   - "produced": "best available full picture" — combined while the
//     working directory exists, then persisted/historical branch-range
//     fallback instead of worktree_gone. Used by the pipeline board's
//     Produced-elements panel.
// Empty string means "let the backend pick the default" (the live
// uncommitted view when a worktree exists, branch otherwise).
export type RunFilesMode = "uncommitted" | "branch" | "combined" | "produced" | "";

// Mirror of server.runFilesResponse. `available` is the gate: when
// false, `reason` is one of "no_workdir" | "not_git_repo" |
// "no_baseline" | "worktree_gone" | "building" and the studio renders
// an empty-state instead of a file list.
//
// `building` is a live run whose worktree/branch-range gitmeta this
// server pod can't yet see (a cloud run's worktree lives on the runner
// pod; its gitmeta is only recorded at finalize) — the panel shows an
// "available when the run finishes" hint rather than an error.
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
    | "building"
    | string;
}

// TouchedFile is one file the run's LLM nodes wrote/edited, derived from
// the persisted tool_started events (mirror of server.touchedFile). Paths
// are workdir-relative when the write landed inside run.WorkDir (same
// namespace as RunFile.path), absolute otherwise.
export interface TouchedFile {
  path: string;
  // Workflow nodes that wrote this path, in first-write order.
  node_ids: string[];
  // Number of write/edit tool calls that targeted this path.
  writes: number;
  // Event seq of the most recent write.
  last_seq: number;
}

// Mirror of server.runTouchedFilesResponse
// (GET /api/runs/{id}/files/touched). Unlike the git-based RunFiles view,
// this is derived purely from run events: it only ever lists what the
// nodes actually wrote — never ambient workspace state — and knows
// nothing about files created by Bash commands or direct tool nodes.
export interface RunTouchedFiles {
  work_dir?: string;
  worktree?: boolean;
  files: TouchedFile[];
}

// Mirror of pkg/git.DiffPayload. before/after are nil for added/deleted
// files respectively; binary suppresses both contents so the UI can
// substitute a "binary file" placeholder. oversized suppresses both
// contents when a side exceeds the diff read cap OR (for a persisted
// cloud diff) the content was dropped past the persistence budget — the
// UI substitutes a "too large to display" placeholder. Status is not part
// of the payload — the caller passes it through from the prior /files
// listing.
export interface RunFileDiff {
  path: string;
  before: string | null;
  after: string | null;
  binary: boolean;
  oversized?: boolean;
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
