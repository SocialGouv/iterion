import type { IterDocument, WorkflowDecl } from "@/api/types";

export type RefContextKind =
  | "edge-with"
  | "prompt-body"
  | "node-prompt"
  | "monaco"
  | "generic";

export interface RefContext {
  kind: RefContextKind;
  /** Source node of an edge — used by edge-with to resolve {{input.*}} from the source output schema. */
  edgeFrom?: string;
  /** Target node of an edge. */
  edgeTo?: string;
  /** The node id whose prompt/instructions field is being edited. */
  nodeId?: string;
}

export type RefGroup = "input" | "vars" | "outputs" | "sessions" | "artifacts";

export interface RefSuggestion {
  /** Display label (e.g. "agent.field"). */
  label: string;
  /** Full template expression to insert (e.g. "{{outputs.agent.field}}"). */
  value: string;
  /** Logical group for sectioned rendering. */
  group: RefGroup;
  /** Optional detail (type, source). */
  detail?: string;
}

/**
 * Locate a node's input schema name across all node kinds.
 * Returns "" if the node doesn't exist or has no input schema.
 */
function findNodeInputSchema(doc: IterDocument, nodeName: string): string {
  for (const a of doc.agents) if (a.name === nodeName) return a.input ?? "";
  for (const j of doc.judges) if (j.name === nodeName) return j.input ?? "";
  for (const h of doc.humans) if (h.name === nodeName) return h.input ?? "";
  for (const t of doc.tools) if (t.name === nodeName) return t.input ?? "";
  for (const c of doc.computes ?? []) if (c.name === nodeName) return c.input ?? "";
  return "";
}

function findNodeOutputSchema(doc: IterDocument, nodeName: string): string {
  for (const a of doc.agents) if (a.name === nodeName) return a.output ?? "";
  for (const j of doc.judges) if (j.name === nodeName) return j.output ?? "";
  for (const h of doc.humans) if (h.name === nodeName) return h.output ?? "";
  for (const t of doc.tools) if (t.name === nodeName) return t.output ?? "";
  for (const c of doc.computes ?? []) if (c.name === nodeName) return c.output ?? "";
  return "";
}

function findRouter(doc: IterDocument, name: string) {
  return (doc.routers ?? []).find((r) => r.name === name);
}

/** Keys a router copies onto its output — mirrors ir.routerPassThroughKeys. */
function routerPassThroughFields(
  doc: IterDocument,
  wf: WorkflowDecl | undefined,
  routerName: string,
): string[] {
  const router = findRouter(doc, routerName);
  if (!router || !wf) return [];
  const keys = new Set<string>();
  for (const e of wf.edges ?? []) {
    if (e.to !== routerName) continue;
    for (const w of e.with ?? []) {
      if (w.key) keys.add(w.key);
    }
  }
  if (wf.entry === routerName) {
    for (const v of doc.vars?.fields ?? []) if (v.name) keys.add(v.name);
    for (const v of wf.vars?.fields ?? []) if (v.name) keys.add(v.name);
  }
  if (router.mode === "llm") {
    keys.add("reasoning");
    keys.add(router.multi ? "selected_routes" : "selected_route");
  }
  if (router.mode === "fan_out_each") {
    keys.add(router.as || "item");
    keys.add("item");
    keys.add("index");
    keys.add("count");
  }
  return [...keys];
}

interface NodeRef {
  name: string;
  output: string;
  delegated: boolean;
}

function collectAllNodes(doc: IterDocument): NodeRef[] {
  const all: NodeRef[] = [];
  for (const a of doc.agents) {
    all.push({ name: a.name, output: a.output ?? "", delegated: !!a.backend });
  }
  for (const j of doc.judges) {
    all.push({ name: j.name, output: j.output ?? "", delegated: !!j.backend });
  }
  for (const h of doc.humans) {
    all.push({ name: h.name, output: h.output ?? "", delegated: false });
  }
  for (const t of doc.tools) {
    all.push({ name: t.name, output: t.output ?? "", delegated: false });
  }
  for (const c of doc.computes ?? []) {
    all.push({ name: c.name, output: c.output ?? "", delegated: false });
  }
  return all;
}

function activeWorkflow(doc: IterDocument, name?: string): WorkflowDecl | undefined {
  const wfs = doc.workflows ?? [];
  if (!wfs.length) return undefined;
  if (name) {
    const found = wfs.find((w) => w.name === name);
    if (found) return found;
  }
  return wfs[0];
}

/**
 * Compute reference suggestions for a given editing context.
 *
 * Generalizes the inline picker that lived inside EdgeForm so every text field
 * (form inputs + Monaco source view) shares one source of truth.
 */
export function computeRefs(
  doc: IterDocument | null,
  ctx: RefContext,
  activeWorkflowName?: string,
): RefSuggestion[] {
  if (!doc) return [];
  const refs: RefSuggestion[] = [];

  // {{input.*}}: in a prompt, the consuming node's input schema; in an
  // edge with-mapping, the SOURCE node's output — schema fields for
  // agents/tools, pass-through keys for routers (incoming with + mode
  // bindings). Destination input fields belong in the mapping KEYS.
  let inputSchemaName = "";
  if (ctx.kind === "edge-with" && ctx.edgeFrom) {
    const wf = activeWorkflow(doc, activeWorkflowName);
    if (findRouter(doc, ctx.edgeFrom)) {
      for (const name of routerPassThroughFields(doc, wf, ctx.edgeFrom)) {
        refs.push({
          label: name,
          value: `{{input.${name}}}`,
          group: "input",
          detail: "router pass-through",
        });
      }
    } else {
      inputSchemaName = findNodeOutputSchema(doc, ctx.edgeFrom);
    }
  } else if (ctx.kind === "node-prompt" && ctx.nodeId) {
    inputSchemaName = findNodeInputSchema(doc, ctx.nodeId);
  }
  if (inputSchemaName) {
    const schema = doc.schemas.find((s) => s.name === inputSchemaName);
    if (schema) {
      for (const f of schema.fields) {
        if (!f.name) continue;
        refs.push({
          label: f.name,
          value: `{{input.${f.name}}}`,
          group: "input",
          detail: f.type,
        });
      }
    }
  }

  // {{vars.*}} top-level + active workflow.
  const seenVars = new Set<string>();
  const topVars = doc.vars?.fields ?? [];
  for (const v of topVars) {
    if (!v.name || seenVars.has(v.name)) continue;
    seenVars.add(v.name);
    refs.push({
      label: v.name,
      value: `{{vars.${v.name}}}`,
      group: "vars",
      detail: v.type,
    });
  }
  const wf = activeWorkflow(doc, activeWorkflowName);
  for (const v of wf?.vars?.fields ?? []) {
    if (!v.name || seenVars.has(v.name)) continue;
    seenVars.add(v.name);
    refs.push({
      label: v.name,
      value: `{{vars.${v.name}}}`,
      group: "vars",
      detail: `${v.type} (workflow)`,
    });
  }

  // {{outputs.*}} from every node + per-field for nodes with output schemas.
  // Plus {{outputs.<delegated>._session_id}} for delegated agents/judges.
  const nodes = collectAllNodes(doc);
  for (const node of nodes) {
    refs.push({
      label: node.name,
      value: `{{outputs.${node.name}}}`,
      group: "outputs",
      detail: node.output ? `→ ${node.output}` : undefined,
    });
    if (node.output) {
      const schema = doc.schemas.find((s) => s.name === node.output);
      if (schema) {
        for (const f of schema.fields) {
          if (!f.name) continue;
          refs.push({
            label: `${node.name}.${f.name}`,
            value: `{{outputs.${node.name}.${f.name}}}`,
            group: "outputs",
            detail: f.type,
          });
        }
      }
    }
    if (node.delegated) {
      refs.push({
        label: `${node.name}._session_id`,
        value: `{{outputs.${node.name}._session_id}}`,
        group: "sessions",
        detail: "session",
      });
    }
  }

  // {{artifacts.*}} from every node with `publish:`.
  const seenArtifacts = new Set<string>();
  const pushArtifact = (publish?: string) => {
    if (!publish || seenArtifacts.has(publish)) return;
    seenArtifacts.add(publish);
    refs.push({
      label: publish,
      value: `{{artifacts.${publish}}}`,
      group: "artifacts",
    });
  };
  for (const a of doc.agents) pushArtifact(a.publish);
  for (const j of doc.judges) pushArtifact(j.publish);
  for (const h of doc.humans) pushArtifact(h.publish);
  for (const t of doc.tools) pushArtifact(t.publish);
  for (const c of doc.computes ?? []) pushArtifact(c.publish);

  return refs;
}

export const REF_GROUP_ORDER: RefGroup[] = [
  "input",
  "vars",
  "outputs",
  "sessions",
  "artifacts",
];

export function groupRefs(refs: RefSuggestion[]): Map<RefGroup, RefSuggestion[]> {
  const map = new Map<RefGroup, RefSuggestion[]>();
  for (const r of refs) {
    if (!map.has(r.group)) map.set(r.group, []);
    map.get(r.group)!.push(r);
  }
  // Reorder map keys following the canonical order.
  const ordered = new Map<RefGroup, RefSuggestion[]>();
  for (const g of REF_GROUP_ORDER) {
    if (map.has(g)) ordered.set(g, map.get(g)!);
  }
  return ordered;
}

/**
 * Score a suggestion against a query for fuzzy filtering.
 * Higher score = better match. Returns null when there's no match.
 */
export function fuzzyScore(query: string, label: string): number | null {
  if (!query) return 0;
  const q = query.toLowerCase();
  const l = label.toLowerCase();
  if (l === q) return 1000;
  if (l.startsWith(q)) return 500 - (l.length - q.length);
  const idx = l.indexOf(q);
  if (idx >= 0) return 250 - idx;
  // Subsequence match (cheap fuzzy).
  let li = 0;
  for (let i = 0; i < q.length; i++) {
    const ch = q[i]!;
    const found = l.indexOf(ch, li);
    if (found < 0) return null;
    li = found + 1;
  }
  return 100 - (li - q.length);
}
