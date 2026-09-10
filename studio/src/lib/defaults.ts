import type {
  IterDocument,
  NodeKind,
  AgentDecl,
  JudgeDecl,
  RouterDecl,
  HumanDecl,
  ToolNodeDecl,
  ComputeDecl,
  SubbotDecl,
  FailDecl,
  SchemaDecl,
  PromptDecl,
} from "@/api/types";

// model and backend are intentionally empty so the runtime resolver
// auto-detects on first execution (see docs/backends.md).
export function createEmptyDocument(): IterDocument {
  return {
    prompts: [
      { name: "system_prompt", body: "You are a helpful assistant." },
      { name: "user_prompt", body: "{{input.query}}" },
    ],
    schemas: [
      { name: "input", fields: [{ name: "query", type: "string" as const }] },
      { name: "output", fields: [{ name: "response", type: "string" as const }] },
    ],
    agents: [{
      name: "agent_1",
      model: "",
      backend: "",
      input: "input",
      output: "output",
      system: "system_prompt",
      user: "user_prompt",
      session: "fresh",
    }],
    judges: [],
    routers: [],
    humans: [],
    tools: [],
    computes: [],
    workflows: [{ name: "main", entry: "agent_1", edges: [{ from: "agent_1", to: "done" }] }],
    comments: [],
  };
}

export function defaultAgent(name: string): AgentDecl {
  return { name, model: "", input: "", output: "", system: "", user: "", session: "fresh" };
}

export function defaultJudge(name: string): JudgeDecl {
  return { name, model: "", input: "", output: "", system: "", user: "", session: "fresh" };
}

export function defaultRouter(name: string): RouterDecl {
  return { name, mode: "fan_out_all" };
}

export function defaultHuman(name: string): HumanDecl {
  // `interaction: "human"` is the canonical default — it's what a bare
  // `human <name>:` block resolves to in the AST. The previous
  // editor-only `pause_until_answers` was a synonym that never reached
  // the wire format.
  return { name, input: "", output: "", instructions: "", interaction: "human" };
}

export function defaultTool(name: string): ToolNodeDecl {
  return { name, command: "", output: "" };
}

export function defaultCompute(name: string): ComputeDecl {
  return { name, output: "", expr: [] };
}

export function defaultSubbot(name: string): SubbotDecl {
  return { name, source: "" };
}

export function defaultSchema(name: string): SchemaDecl {
  return { name, fields: [] };
}

export function defaultPrompt(name: string): PromptDecl {
  return { name, body: "" };
}

export function getAllSchemaNames(doc: IterDocument): Set<string> {
  return new Set((doc.schemas ?? []).map((s) => s.name));
}

export function getAllPromptNames(doc: IterDocument): Set<string> {
  return new Set((doc.prompts ?? []).map((p) => p.name));
}

export function generateUniqueName(base: string, existingNames: Set<string>): string {
  let i = 1;
  while (existingNames.has(`${base}_${i}`)) i++;
  return `${base}_${i}`;
}

/** Find a node declaration by name across all node type arrays. */
export function findNodeDecl(
  doc: IterDocument,
  name: string,
):
  | { kind: NodeKind; decl: AgentDecl | JudgeDecl | RouterDecl | HumanDecl | ToolNodeDecl | ComputeDecl | SubbotDecl | FailDecl }
  | null {
  const agent = doc.agents?.find((a) => a.name === name);
  if (agent) return { kind: "agent", decl: agent };
  const judge = doc.judges?.find((j) => j.name === name);
  if (judge) return { kind: "judge", decl: judge };
  const router = doc.routers?.find((r) => r.name === name);
  if (router) return { kind: "router", decl: router };
  const human = doc.humans?.find((h) => h.name === name);
  if (human) return { kind: "human", decl: human };
  const tool = doc.tools?.find((t) => t.name === name);
  if (tool) return { kind: "tool", decl: tool };
  const compute = doc.computes?.find((c) => c.name === name);
  if (compute) return { kind: "compute", decl: compute };
  const subbot = doc.subbots?.find((sb) => sb.name === name);
  if (subbot) return { kind: "subbot", decl: subbot };
  // A named `fail <name>:` IS a graph node: the canvas draws it, so every
  // surface that resolves a node by name has to know it too, or the
  // inspector reports the node it just rendered as "not found".
  const fail = doc.fails?.find((f) => f.name === name);
  if (fail) return { kind: "fail", decl: fail };
  return null;
}

export function getAllNodeNames(doc: IterDocument): Set<string> {
  const names = new Set<string>();
  for (const a of doc.agents) names.add(a.name);
  for (const j of doc.judges) names.add(j.name);
  for (const r of doc.routers) names.add(r.name);
  for (const h of doc.humans) names.add(h.name);
  for (const t of doc.tools) names.add(t.name);
  for (const c of doc.computes ?? []) names.add(c.name);
  for (const sb of doc.subbots ?? []) names.add(sb.name);
  // Without this a user can name an agent after a declared fail node and
  // only discover the collision at compile time (C041).
  for (const f of doc.fails ?? []) names.add(f.name);
  return names;
}
