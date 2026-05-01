// Mirror of pkg/dsl/ast/jsonenc.go (the wire format for the editor's
// document round-trip). Field shapes and JSON tags are kept aligned with
// the Go side; reload the editor whenever the AST changes.

export interface IterDocument {
  vars?: VarsBlock;
  mcp_servers?: MCPServerDecl[];
  prompts: PromptDecl[];
  schemas: SchemaDecl[];
  agents: AgentDecl[];
  judges: JudgeDecl[];
  routers: RouterDecl[];
  humans: HumanDecl[];
  tools: ToolNodeDecl[];
  computes: ComputeDecl[];
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

export type ReasoningEffort = "low" | "medium" | "high" | "extra_high";

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
}

export type RouterMode = "fan_out_all" | "condition" | "round_robin" | "llm";

export interface RouterDecl {
  name: string;
  mode: RouterMode;
  model?: string;
  backend?: string;
  system?: string;
  user?: string;
  multi?: boolean;
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
  await?: AwaitMode;
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
  await?: AwaitMode;
}

export interface ComputeExpr {
  // Output schema field name receiving the expression result.
  key: string;
  // Raw expression source, e.g. `input.count >= vars.loop_count`.
  expr: string;
}

// ---------------------------------------------------------------------------
// Workflow
// ---------------------------------------------------------------------------

export interface WorkflowDecl {
  name: string;
  vars?: VarsBlock;
  entry: string;
  default_backend?: string;
  tool_policy?: string[];
  mcp?: MCPConfigDecl;
  budget?: BudgetBlock;
  compaction?: CompactionBlock;
  interaction?: InteractionMode;
  edges: Edge[];
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

export interface LoopClause {
  name: string;
  max_iterations: number;
}

export interface WithEntry {
  key: string;
  value: string;
}

// Node kind for the visual editor. Includes the deterministic
// `compute` node and the synthetic terminals (`done`/`fail`/`start`)
// used for canvas rendering only — they don't have AST declarations.
export type NodeKind =
  | "agent"
  | "judge"
  | "router"
  | "human"
  | "tool"
  | "compute"
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
