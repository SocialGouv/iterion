import type {
  AgentDecl,
  JudgeDecl,
  RouterDecl,
  HumanDecl,
  ToolNodeDecl,
  ComputeDecl,
  SchemaDecl,
  PromptDecl,
  VarField,
  WhenClause,
  LoopClause,
  WithEntry,
} from "@/api/types";

/** Categories for library items: node kinds (minus terminal) + primitive kinds + pattern */
export type LibraryCategory =
  | "agent" | "judge" | "router" | "human" | "tool" | "compute"
  | "schema" | "prompt" | "var"
  | "pattern";

/** Discriminated union so TypeScript can narrow the node data properly */
export type NodeTemplate =
  | { kind: "agent"; data: Omit<Partial<AgentDecl>, "name"> }
  | { kind: "judge"; data: Omit<Partial<JudgeDecl>, "name"> }
  | { kind: "router"; data: Omit<Partial<RouterDecl>, "name"> }
  | { kind: "human"; data: Omit<Partial<HumanDecl>, "name"> }
  | { kind: "tool"; data: Omit<Partial<ToolNodeDecl>, "name"> }
  | { kind: "compute"; data: Omit<Partial<ComputeDecl>, "name"> };

/** Edge template for multi-node patterns, using placeholder names. */
export interface EdgeTemplate {
  from: string;
  to: string;
  when?: WhenClause;
  loop?: LoopClause;
  with?: WithEntry[];
}

/** A node entry within a pattern, keyed by placeholder for edge remapping. */
export interface PatternNodeEntry {
  placeholder: string;
  node: NodeTemplate;
  schemas?: SchemaDecl[];
  prompts?: PromptDecl[];
  vars?: VarField[];
}

export interface LibraryItem {
  id: string;
  name: string;
  description: string;
  category: LibraryCategory;
  tags?: string[];
  builtin: boolean;
  template: {
    node?: NodeTemplate;
    schemas?: SchemaDecl[];
    prompts?: PromptDecl[];
    vars?: VarField[];
    /** Multi-node pattern (mutually exclusive with node). */
    pattern?: {
      nodes: PatternNodeEntry[];
      edges: EdgeTemplate[];
      groupName?: string;
    };
  };
}
