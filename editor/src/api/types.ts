// Mirror of ast.File
export interface IterDocument {
  vars?: VarsBlock;
  prompts: PromptDecl[];
  schemas: SchemaDecl[];
  agents: AgentDecl[];
  judges: JudgeDecl[];
  routers: RouterDecl[];
  joins: JoinDecl[];
  humans: HumanDecl[];
  tools: ToolNodeDecl[];
  workflows: WorkflowDecl[];
  comments: Comment[];
}

export interface Comment { text: string; }
export interface VarsBlock { fields: VarField[]; }
export interface VarField { name: string; type: TypeExpr; default?: Literal; }
export type TypeExpr = "string" | "bool" | "int" | "float" | "json" | "string[]";
export interface Literal { kind: LiteralKind; raw: string; str_val?: string; int_val?: number; float_val?: number; bool_val?: boolean; }
export type LiteralKind = "string" | "int" | "float" | "bool";

export interface PromptDecl { name: string; body: string; }
export interface SchemaDecl { name: string; fields: SchemaField[]; }
export interface SchemaField { name: string; type: FieldType; enum_values?: string[]; }
export type FieldType = "string" | "bool" | "int" | "float" | "json" | "string[]";

export type SessionMode = "fresh" | "inherit" | "fork" | "artifacts_only";
export interface AgentDecl {
  name: string; model: string; delegate?: string;
  input: string; output: string; publish?: string;
  system: string; user: string; session: SessionMode;
  tools?: string[]; tool_max_steps?: number;
}
export interface JudgeDecl {
  name: string; model: string; delegate?: string;
  input: string; output: string; publish?: string;
  system: string; user: string; session: SessionMode;
  tools?: string[]; tool_max_steps?: number;
}

export type RouterMode = "fan_out_all" | "condition" | "round_robin" | "llm";
export interface RouterDecl {
  name: string; mode: RouterMode;
  model?: string; system?: string; user?: string; multi?: boolean;
}

export type JoinStrategy = "wait_all" | "best_effort";
export interface JoinDecl { name: string; strategy: JoinStrategy; require: string[]; output: string; }

export type HumanMode = "pause_until_answers" | "auto_answer" | "auto_or_pause";
export interface HumanDecl {
  name: string; input: string; output: string; publish?: string;
  instructions: string; mode: HumanMode; min_answers?: number;
  model?: string; system?: string;
}

export interface ToolNodeDecl { name: string; command: string; output: string; }

export interface WorkflowDecl {
  name: string; vars?: VarsBlock; entry: string;
  budget?: BudgetBlock; edges: Edge[];
}
export interface BudgetBlock {
  max_parallel_branches?: number; max_duration?: string;
  max_cost_usd?: number; max_tokens?: number; max_iterations?: number;
}
export interface Edge {
  from: string; to: string;
  when?: WhenClause; loop?: LoopClause; with?: WithEntry[];
}
export interface WhenClause { condition: string; negated: boolean; }
export interface LoopClause { name: string; max_iterations: number; }
export interface WithEntry { key: string; value: string; }

// Node kind for the visual editor
export type NodeKind = "agent" | "judge" | "router" | "join" | "human" | "tool" | "done" | "fail" | "start";

// File management types
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
