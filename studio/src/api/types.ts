// Mirror of pkg/dsl/ast/jsonenc.go (the wire format for the studio's
// document round-trip). Field shapes and JSON tags are kept aligned with
// the Go side; reload the studio whenever the AST changes.

export interface IterDocument {
  vars?: VarsBlock;
  presets?: PresetsBlock;
  attachments?: AttachmentsBlock;
  mcp_servers?: MCPServerDecl[];
  prompts: PromptDecl[];
  schemas: SchemaDecl[];
  cursors?: CursorDecl[];
  agents: AgentDecl[];
  judges: JudgeDecl[];
  routers: RouterDecl[];
  humans: HumanDecl[];
  tools: ToolNodeDecl[];
  computes: ComputeDecl[];
  subbots?: SubbotDecl[];
  workflows: WorkflowDecl[];
  comments: Comment[];
}

export interface Comment {
  text: string;
}

// ---------------------------------------------------------------------------
// Vars
// ---------------------------------------------------------------------------

export interface VarsBlock {
  fields: VarField[];
}

export interface VarField {
  name: string;
  type: TypeExpr;
  default?: Literal;
  /** DSL `[enum: "a", "b"]` constraint — present only on string vars.
   *  Non-empty ⇒ the var renders as a fixed-choice select. */
  enum?: string[];
}

export type TypeExpr = "string" | "bool" | "int" | "float" | "json" | "string[]";

export interface Literal {
  kind: LiteralKind;
  raw: string;
  str_val?: string;
  int_val?: number;
  float_val?: number;
  bool_val?: boolean;
}

export type LiteralKind = "string" | "int" | "float" | "bool";

// ---------------------------------------------------------------------------
// Presets — named bundles of variable values selected at launch time.
// Mirrors pkg/dsl/ast.PresetsBlock.
// ---------------------------------------------------------------------------

export interface PresetsBlock {
  entries: Preset[];
}

export interface Preset {
  name: string;
  // Rich metadata, present only for file-based presets (a bundle's
  // presets/<name>.md). In-source `presets:` entries set only name + values.
  display_name?: string;
  description?: string;
  prompt?: string;
  skills?: string[];
  values: PresetValue[];
}

export interface PresetValue {
  key: string;
  value: Literal;
}

// ---------------------------------------------------------------------------
// Attachments
// ---------------------------------------------------------------------------

export type AttachmentType = "file" | "image";

export interface AttachmentsBlock {
  fields: AttachmentField[];
}

export interface AttachmentField {
  name: string;
  type: AttachmentType;
  required?: boolean;
  accept_mime?: string[];
  description?: string;
}

// Server-info upload limits (mirrors pkg/server.uploadLimitsBlock).
export interface UploadLimits {
  max_file_size: number;
  max_total_size: number;
  max_files_per_run: number;
  allowed_mime: string[];
}

export interface ServerInfo {
  mode: string;
  version: string;
  commit?: string;
  limits: { upload: UploadLimits };
  // Absolute working directory the server was launched with. Empty in
  // cloud mode (no per-server folder concept).
  work_dir?: string;
  // Human-friendly label derived from work_dir (basename). Empty when
  // work_dir is empty or root-ish.
  project_name?: string;
  // ID of the project entry from the registry that matches `work_dir`
  // (set by ProjectSwitcher). Empty when the registry has never been
  // written or in cloud mode.
  current_project_id?: string;
  // Absolute path of the server-side directory browser root. Empty
  // when ITERION_BROWSE_ROOT is unset — the SPA shows the Browse
  // button in AddProjectDialog only when this is non-empty.
  browse_root?: string;
  // native_tracker_enabled is true when the studio server has the
  // kanban store wired. The SPA exposes the Board view conditionally.
  native_tracker_enabled?: boolean;
  // dispatcher_enabled is true when a long-running dispatcher is
  // attached. The SPA exposes the Dispatcher dashboard conditionally.
  dispatcher_enabled?: boolean;
  // cost_cap_enabled is true when a per-(store, UTC-day) LLM spend cap
  // is configured. The SPA polls GET /api/v1/limits/cost for live status
  // and renders the cost-cap banner only when this is true.
  cost_cap_enabled?: boolean;
  // auth_required is false in local / desktop mode (single-user TTY,
  // no JWT) and true in cloud mode (multi-tenant). The SettingsPage
  // gates auth-only tabs (Tokens, change password) on this.
  auth_required?: boolean;
  // email_enabled is true when an SMTP mailer is wired. The SPA only
  // offers email-dependent flows (forgot-password link, "send invite
  // by email") when true.
  email_enabled?: boolean;
  // marketplace_enabled is true when the hosted bot registry is wired.
  // The SPA exposes the Marketplace view + nav entry conditionally.
  marketplace_enabled?: boolean;
  // triggers_enabled is true when the event-driven trigger store is wired.
  // The SPA exposes the Automations (Triggers) view + nav entry conditionally.
  triggers_enabled?: boolean;
  // config_shares_enabled is true when the scoped config-share editor is wired
  // (store + generic-secret stack + auth). The SPA exposes the operator "Share
  // config" surface on the bot home when set; the shell-less /config/:id
  // editor route is always mounted (a public link renders its own error state
  // when the share is unknown or the feature is off).
  config_shares_enabled?: boolean;
  // plugins_enabled is always true: the plugin registry is available in every
  // mode, so the SPA can surface the Plugins management view + nav entry.
  plugins_enabled?: boolean;
  // bot_editing_enabled is true when the .bot editor + /bots/new builder +
  // "Duplicate & edit" fork are available. Always true in local mode (edits the
  // real filesystem); in cloud mode true only when the team-authored bot store
  // is wired. Gates the Editor nav entry and the per-bot edit affordances.
  bot_editing_enabled?: boolean;
  // web_push_enabled is true when the server can deliver browser push
  // notifications (VAPID keys + subscription store wired). Gates the
  // Notifications settings panel.
  web_push_enabled?: boolean;
  // web_push_vapid_public_key is the applicationServerKey this browser
  // subscribes with (public by design).
  web_push_vapid_public_key?: string;
  // secrets_enabled is true in local (non-cloud) mode when a sealed secret
  // store + sealer are wired. The SPA exposes the local Secrets view + nav
  // entry conditionally. Cloud mode uses the auth-gated team/personal secrets
  // UI instead (this flag stays false there).
  secrets_enabled?: boolean;
  // run_shell_enabled is true in local (non-cloud) mode: the run view
  // offers the post-mortem shell (GET /api/ws/runs/{id}/shell) on
  // terminal runs whose preserved worktree still exists.
  run_shell_enabled?: boolean;
  // skills_enabled is true in local (non-cloud) mode: the SPA surfaces the
  // Skills library management view + nav entry. No sealing, so it gates on
  // mode alone.
  skills_enabled?: boolean;
  // forge_github_app_configured is true when the server has a GitHub App
  // configured (ITERION_FORGE_GITHUB_APP_*). The forge ConnectForm only offers
  // the "Install GitHub App" mode when true — otherwise it dead-ends on a 400.
  // OAuth / PAT connect modes don't depend on it.
  forge_github_app_configured?: boolean;
  // session_board_enabled is true when the LLM Session-board curation layer
  // is on for this server (ITERION_SESSION_BOARD). The Tasks tab only
  // fetches the curated widget spec when true; otherwise it shows the
  // deterministic task-list board alone and never polls.
  session_board_enabled?: boolean;
  // pipeline_concurrency reports the local pipeline-concurrency gate (the
  // global pipeline board's TODO/in-flight admission). Mirrors the board's
  // own `concurrency` block; present so surfaces outside the board (nav,
  // status pills) can read the cap without polling the board.
  pipeline_concurrency?: PipelineConcurrencyInfo;
}

// PipelineConcurrencyInfo mirrors runview.PipelineConcurrencyStatus. When
// `enabled` is false the other fields are zero and the pipeline TODO lane
// only holds not-yet-launched native tasks.
export interface PipelineConcurrencyInfo {
  enabled: boolean;
  max: number;
  active: number;
  waiting: number;
}

// CostCapStatus mirrors runtime.CapStatus (GET /api/v1/limits/cost).
export interface CostCapStatus {
  enabled: boolean;
  date: string;
  spent_usd: number;
  limit_usd: number;
  exceeded: boolean;
  override_active: boolean;
}

// Response shape of POST /api/runs/uploads.
export interface StagedUpload {
  upload_id: string;
  original_filename: string;
  mime: string;
  size: number;
  sha256: string;
  created_at: string;
}

// ---------------------------------------------------------------------------
// MCP
// ---------------------------------------------------------------------------

export type MCPTransport = "unknown" | "stdio" | "http" | "sse";

export interface MCPServerDecl {
  name: string;
  transport?: MCPTransport;
  command?: string;
  args?: string[];
  url?: string;
  auth?: MCPAuthDecl;
}

export interface MCPAuthDecl {
  type?: string;
  auth_url?: string;
  token_url?: string;
  revoke_url?: string;
  client_id?: string;
  scopes?: string[];
}

export interface MCPConfigDecl {
  autoload_project?: boolean;
  inherit?: boolean;
  servers?: string[];
  disable?: string[];
}

// ---------------------------------------------------------------------------
// Compaction
// ---------------------------------------------------------------------------

export interface CompactionBlock {
  // Ratio of model context window (0 < t <= 1); omit to inherit.
  threshold?: number;
  // Recent messages kept verbatim (>= 1); omit to inherit.
  preserve_recent?: number;
}

// ---------------------------------------------------------------------------
// Prompts / Schemas
// ---------------------------------------------------------------------------

export interface PromptDecl {
  name: string;
  body: string;
}

export interface SchemaDecl {
  name: string;
  fields: SchemaField[];
}

export interface SchemaField {
  name: string;
  type: FieldType;
  enum_values?: string[];
}

export type FieldType = "string" | "bool" | "int" | "float" | "json" | "string[]";

// ---------------------------------------------------------------------------
// Nodes
// ---------------------------------------------------------------------------

export type SessionMode = "fresh" | "inherit" | "fork" | "artifacts_only";
export type AwaitMode = "none" | "wait_all" | "best_effort";

// InteractionMode is unified across agent/judge/human nodes. Replaces
// the old editor-only `HumanMode` and adds llm/llm_or_human surfaces.
export type InteractionMode = "none" | "human" | "llm" | "llm_or_human";

// "ultracode" is a mode, not a wire effort: xhigh + a standing prerogative to
// orchestrate multi-agent workflows (reliable only on claude-opus-4-8). The
// runtime remaps it to xhigh before the provider. See docs/ultracode.md.
export type ReasoningEffort = "low" | "medium" | "high" | "xhigh" | "max" | "ultracode";

export interface AgentDecl {
  name: string;
  model: string;
  // Execution backend name (e.g. "claude_code", "codex", "claw"). When
  // set, bypasses the direct LLM API path. Aligned with the Go AST's
  // `Backend` field — the previous editor-only `delegate` is gone.
  backend?: string;
  mcp?: MCPConfigDecl;
  input: string;
  output: string;
  publish?: string;
  system: string;
  user: string;
  session: SessionMode;
  tools?: string[];
  tool_policy?: string[];
  tool_max_steps?: number;
  // Per-LLM-call output cap; 0 / undefined inherits the backend default.
  max_tokens?: number;
  reasoning_effort?: ReasoningEffort;
  readonly?: boolean;
  interaction?: InteractionMode;
  interaction_prompt?: string;
  interaction_model?: string;
  await?: AwaitMode;
  compaction?: CompactionBlock;
  cursors?: CursorBlock;
  // Resource names this node acquires before running (workflow.resources).
  needs?: string[];
}

export interface JudgeDecl {
  name: string;
  model: string;
  backend?: string;
  mcp?: MCPConfigDecl;
  input: string;
  output: string;
  publish?: string;
  system: string;
  user: string;
  session: SessionMode;
  tools?: string[];
  tool_policy?: string[];
  tool_max_steps?: number;
  max_tokens?: number;
  reasoning_effort?: ReasoningEffort;
  readonly?: boolean;
  interaction?: InteractionMode;
  interaction_prompt?: string;
  interaction_model?: string;
  await?: AwaitMode;
  compaction?: CompactionBlock;
  cursors?: CursorBlock;
  // Resource names this node acquires before running (workflow.resources).
  needs?: string[];
}

// ---------------------------------------------------------------------------
// Cursors (prompt-engineering dials, see docs/cursors.md)
// ---------------------------------------------------------------------------

export interface CursorDecl {
  name: string;
  description?: string;
  values?: CursorEnumValue[];
  bands?: CursorBand[];
}

export interface CursorEnumValue {
  name: string;
  prompt: string;
}

export interface CursorBand {
  range: string; // "lo..hi", lo & hi in [0,1]
  prompt: string;
}

export interface CursorBlock {
  enabled: boolean;
  settings?: CursorSetting[];
}

export interface CursorSetting {
  key: string;
  value: string;
}

export type WorktreeMode = "auto" | "none";

export type RouterMode =
  | "fan_out_all"
  | "fan_out_each"
  | "condition"
  | "round_robin"
  | "llm";

export interface RouterDecl {
  name: string;
  mode: RouterMode;
  model?: string;
  backend?: string;
  system?: string;
  user?: string;
  multi?: boolean;
  reasoning_effort?: ReasoningEffort;
  // fan_out_each: data-driven fan-out. `over` resolves to a runtime array;
  // the single outgoing template subgraph is re-executed once per element,
  // scheduled as a DAG when `key`/`depends_on` are set.
  over?: string;
  as?: string;
  key?: string;
  depends_on?: string;
  // Resource names this node acquires before running (workflow.resources).
  needs?: string[];
}

export interface HumanDecl {
  name: string;
  input: string;
  output: string;
  publish?: string;
  instructions: string;
  // Use `interaction` (unified mode) — the legacy editor-only `mode`
  // field has been removed because it never matched the JSON wire
  // format and was silently dropped on save.
  interaction?: InteractionMode;
  interaction_prompt?: string;
  interaction_model?: string;
  min_answers?: number;
  model?: string;
  system?: string;
  await?: AwaitMode;
}

export interface ToolNodeDecl {
  name: string;
  command: string;
  // Optional input schema reference; lets the tool consume structured
  // data rendered into the command via `{{input.field}}` templates.
  input?: string;
  output: string;
  // Optional. Publishes the node's output as a persistent artifact,
  // referenceable downstream via `{{artifacts.name}}`.
  publish?: string;
  await?: AwaitMode;
  // Resource names this node acquires before running (workflow.resources).
  needs?: string[];
}

// ComputeDecl is a deterministic node: evaluates a list of expressions
// over vars/input/outputs/artifacts/loop/run namespaces and emits a
// structured output. No LLM, no shell — useful for streak detection,
// boolean ANDs, counters, and other plain computation that shouldn't
// burn tokens.
export interface ComputeDecl {
  name: string;
  input?: string;
  output: string;
  expr: ComputeExpr[];
  // Optional. Publishes the node's output as a persistent artifact,
  // referenceable downstream via `{{artifacts.name}}`.
  publish?: string;
  await?: AwaitMode;
}

export interface ComputeExpr {
  // Output schema field name receiving the expression result.
  key: string;
  // Raw expression source, e.g. `input.count >= vars.loop_count`.
  expr: string;
}

// SubbotDecl runs another .bot file as a separate child run. Mirrors
// pkg/dsl/ast/jsonenc.go jsonSubbotDecl: all fields omitempty except name;
// `with` reuses the {key,value} shape of edge data mappings.
export interface SubbotDecl {
  name: string;
  source?: string;
  with?: WithEntry[];
  output?: string;
  needs?: string[];
  isolated?: boolean;
}

// ---------------------------------------------------------------------------
// Workflow
// ---------------------------------------------------------------------------

export interface WorkflowDecl {
  name: string;
  vars?: VarsBlock;
  attachments?: AttachmentsBlock;
  entry: string;
  default_backend?: string;
  tool_policy?: string[];
  mcp?: MCPConfigDecl;
  budget?: BudgetBlock;
  // Named counting semaphores (resource name → capacity). A node that
  // declares `needs: <name>` acquires a slot before running and releases
  // it after — bounds concurrent use of a scarce shared resource (e.g.
  // Godot/Blender sessions) without a global parallelism cap.
  resources?: Record<string, number>;
  compaction?: CompactionBlock;
  interaction?: InteractionMode;
  // Worktree isolation mode. Omit or set to "none" to run in place;
  // "auto" runs the workflow inside a per-run git worktree.
  worktree?: WorktreeMode;
  // Sandbox declaration. Omit / mode: "none" → tools and shell run on
  // the host. mode: "auto" / "inline" + an image → container-isolated
  // per-run. Mirrors pkg/dsl/ir/sandbox.go SandboxSpec — only the
  // fields the studio currently reads are typed (mode + image for
  // active-state detection; build for build-vs-pull surfacing).
  sandbox?: SandboxDecl;
  edges: Edge[];
}

export interface SandboxDecl {
  mode?: "none" | "auto" | "inline" | string;
  image?: string;
  build?: { dockerfile?: string; context?: string };
}

export interface BudgetBlock {
  max_parallel_branches?: number;
  max_duration?: string;
  max_cost_usd?: number;
  max_tokens?: number;
  max_iterations?: number;
}

export interface Edge {
  from: string;
  to: string;
  when?: WhenClause;
  loop?: LoopClause;
  with?: WithEntry[];
}

// WhenClause supports two mutually-exclusive forms:
//   - simple boolean field: { condition: "approved", negated?: true }
//   - raw expression:        { expr: "approved && loop.l.previous.x" }
// The simple form is preserved for ergonomics; the expression form
// unlocks compound conditions evaluated by the compiler.
export interface WhenClause {
  condition?: string;
  negated?: boolean;
  expr?: string;
}

// The AST JSON encoder (pkg/dsl/ast/jsonenc.go) marshals every bound
// field with omitempty: a literal bound arrives as `max_iterations`, a
// template bound (`as name("{{...}}")`) as `max_iterations_expr`, and
// the unbounded form (`as name(unbounded [fuel])`) as `unbounded` (+
// optional `fuel_cap`). Consumers must handle an absent bound instead
// of assuming a number.
export interface LoopClause {
  name: string;
  max_iterations?: number;
  max_iterations_expr?: string;
  unbounded?: boolean;
  fuel_cap?: number;
}

export interface WithEntry {
  key: string;
  value: string;
}

// Node kind for the visual editor. Includes the deterministic
// `compute` node, the `subbot` child-workflow node, and the synthetic
// terminals (`done`/`fail`/`start`) used for canvas rendering only —
// they don't have AST declarations.
export type NodeKind =
  | "agent"
  | "judge"
  | "router"
  | "human"
  | "tool"
  | "compute"
  | "subbot"
  | "done"
  | "fail"
  | "start";

// ---------------------------------------------------------------------------
// File management
// ---------------------------------------------------------------------------

export interface FileEntry {
  name: string;
  size: number;
}

export interface ListFilesResponse {
  files: FileEntry[];
}

export interface SaveFileResponse {
  path: string;
  source: string;
}

// WebSocket file watching events
export interface FileEvent {
  type: "file_created" | "file_modified" | "file_deleted";
  path: string;
}

// Global server-pushed event signalling a project hot-swap. Shares the
// same WebSocket channel as FileEvent (/api/ws); consumers discriminate
// on `type`. The `current` payload lets the SPA refresh the active
// label without a follow-up HTTP round-trip.
export interface ProjectSwitchedEvent {
  type: "project_switched";
  current: {
    id: string;
    name: string;
    dir: string;
    store_dir?: string;
    last_opened: string;
    color?: string;
  };
}

export type ServerWsEvent = FileEvent | ProjectSwitchedEvent;
