import type {
  IterDocument,
  AgentDecl,
  JudgeDecl,
  RouterDecl,
  JoinDecl,
  HumanDecl,
  ToolNodeDecl,
  SchemaDecl,
  PromptDecl,
} from "@/api/types";

export function createEmptyDocument(): IterDocument {
  return {
    prompts: [],
    schemas: [],
    agents: [],
    judges: [],
    routers: [],
    joins: [],
    humans: [],
    tools: [],
    workflows: [{ name: "main", entry: "", edges: [] }],
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

export function defaultJoin(name: string): JoinDecl {
  return { name, strategy: "wait_all", require: [], output: "" };
}

export function defaultHuman(name: string): HumanDecl {
  return { name, input: "", output: "", instructions: "", mode: "pause_until_answers" };
}

export function defaultTool(name: string): ToolNodeDecl {
  return { name, command: "", output: "" };
}

export function defaultSchema(name: string): SchemaDecl {
  return { name, fields: [] };
}

export function defaultPrompt(name: string): PromptDecl {
  return { name, body: "" };
}

export function generateUniqueName(base: string, existingNames: Set<string>): string {
  let i = 1;
  while (existingNames.has(`${base}_${i}`)) i++;
  return `${base}_${i}`;
}

export function getAllNodeNames(doc: IterDocument): Set<string> {
  const names = new Set<string>();
  for (const a of doc.agents) names.add(a.name);
  for (const j of doc.judges) names.add(j.name);
  for (const r of doc.routers) names.add(r.name);
  for (const j of doc.joins) names.add(j.name);
  for (const h of doc.humans) names.add(h.name);
  for (const t of doc.tools) names.add(t.name);
  return names;
}
