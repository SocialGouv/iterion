import type {
  AgentDecl,
  JudgeDecl,
  RouterDecl,
  HumanDecl,
  ToolNodeDecl,
  SchemaDecl,
  PromptDecl,
  VarField,
} from "@/api/types";

/** Categories for library items: node kinds (minus terminal) + primitive kinds */
export type LibraryCategory =
  | "agent" | "judge" | "router" | "human" | "tool"
  | "schema" | "prompt" | "var";

/** Discriminated union so TypeScript can narrow the node data properly */
export type NodeTemplate =
  | { kind: "agent"; data: Omit<Partial<AgentDecl>, "name"> }
  | { kind: "judge"; data: Omit<Partial<JudgeDecl>, "name"> }
  | { kind: "router"; data: Omit<Partial<RouterDecl>, "name"> }
  | { kind: "human"; data: Omit<Partial<HumanDecl>, "name"> }
  | { kind: "tool"; data: Omit<Partial<ToolNodeDecl>, "name"> };

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
  };
}
