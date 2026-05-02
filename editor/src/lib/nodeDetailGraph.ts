import type { Node, Edge as FlowEdge } from "@xyflow/react";
import { MarkerType } from "@xyflow/react";
import type { IterDocument, AgentDecl, JudgeDecl, HumanDecl, ToolNodeDecl, RouterDecl } from "@/api/types";
import { findNodeDecl } from "@/lib/defaults";
import { NODE_COLORS, SUB_COLORS } from "@/lib/constants";
import type { DetailSubNodeData } from "@/components/Canvas/DetailSubNode";

// Prefixes for detail sub-node IDs
export const DETAIL_PREFIX_SCHEMA = "__detail_schema__:";
export const DETAIL_PREFIX_PROMPT = "__detail_prompt__:";
export const DETAIL_PREFIX_VAR = "__detail_var__:";
export const DETAIL_PREFIX_EDGE = "__detail_edge__:";
export const DETAIL_PREFIX_TOOL = "__detail_tool__:";
export const DETAIL_PREFIX_CENTRAL = "__detail_central__";

export function isDetailNodeId(id: string): boolean {
  return (
    id.startsWith(DETAIL_PREFIX_SCHEMA) ||
    id.startsWith(DETAIL_PREFIX_PROMPT) ||
    id.startsWith(DETAIL_PREFIX_VAR) ||
    id.startsWith(DETAIL_PREFIX_EDGE) ||
    id.startsWith(DETAIL_PREFIX_TOOL) ||
    id === DETAIL_PREFIX_CENTRAL
  );
}

export type ParsedDetailId =
  | { kind: "central" }
  | { kind: "schema" | "prompt"; name: string; relation?: string }
  | { kind: "var" | "tool"; name: string }
  | { kind: "edge"; workflowName: string; edgeIndex: number };

export function parseDetailId(id: string): ParsedDetailId | null {
  if (id === DETAIL_PREFIX_CENTRAL) return { kind: "central" };
  if (id.startsWith(DETAIL_PREFIX_SCHEMA)) {
    const rest = id.slice(DETAIL_PREFIX_SCHEMA.length);
    const colonIdx = rest.lastIndexOf(":");
    if (colonIdx >= 0) return { kind: "schema", name: rest.slice(0, colonIdx), relation: rest.slice(colonIdx + 1) };
    return { kind: "schema", name: rest };
  }
  if (id.startsWith(DETAIL_PREFIX_PROMPT)) {
    const rest = id.slice(DETAIL_PREFIX_PROMPT.length);
    const colonIdx = rest.lastIndexOf(":");
    if (colonIdx >= 0) return { kind: "prompt", name: rest.slice(0, colonIdx), relation: rest.slice(colonIdx + 1) };
    return { kind: "prompt", name: rest };
  }
  if (id.startsWith(DETAIL_PREFIX_VAR)) return { kind: "var", name: id.slice(DETAIL_PREFIX_VAR.length) };
  if (id.startsWith(DETAIL_PREFIX_TOOL)) return { kind: "tool", name: id.slice(DETAIL_PREFIX_TOOL.length) };
  if (id.startsWith(DETAIL_PREFIX_EDGE)) {
    const rest = id.slice(DETAIL_PREFIX_EDGE.length);
    const colonIdx = rest.lastIndexOf(":");
    if (colonIdx < 0) return null;
    const workflowName = rest.slice(0, colonIdx);
    const indexText = rest.slice(colonIdx + 1);
    const edgeIndex = Number(indexText);
    if (!workflowName || !/^\d+$/.test(indexText) || !Number.isInteger(edgeIndex)) return null;
    return { kind: "edge", workflowName, edgeIndex };
  }
  return null;
}


function refMarker(color: string) {
  return { type: MarkerType.ArrowClosed as const, color, width: 12, height: 12 };
}

export function generateNodeDetailGraph(
  doc: IterDocument,
  nodeId: string,
  workflowName: string,
): { nodes: Node[]; edges: FlowEdge[] } {
  const nodes: Node[] = [];
  const edges: FlowEdge[] = [];

  const found = findNodeDecl(doc, nodeId);
  if (!found) return { nodes, edges };
  const { kind, decl } = found;

  // 1. Central node — the node itself as a workflowNode
  const color = NODE_COLORS[kind] ?? "#6B7280";
  nodes.push({
    id: DETAIL_PREFIX_CENTRAL,
    type: "workflowNode",
    position: { x: 0, y: 0 },
    data: {
      label: nodeId,
      kind,
      color,
      decl,
    },
  });

  // Helper to get agent-like properties
  const agent = (kind === "agent" || kind === "judge") ? decl as AgentDecl | JudgeDecl : undefined;
  const human = kind === "human" ? decl as HumanDecl : undefined;
  const tool = kind === "tool" ? decl as ToolNodeDecl : undefined;
  const router = kind === "router" ? decl as RouterDecl : undefined;

  // 2. Schema sub-nodes
  const inputSchemaName = (agent || human)?.input;
  const outputSchemaName = (agent || human || tool)?.output;

  if (inputSchemaName) {
    const schema = doc.schemas?.find((s) => s.name === inputSchemaName);
    const schemaId = DETAIL_PREFIX_SCHEMA + inputSchemaName + ":input";
    nodes.push({
      id: schemaId,
      type: "detailSubNode",
      position: { x: 0, y: 0 },
      data: {
        subKind: "schema",
        label: inputSchemaName,
        subtitle: schema ? schema.fields.map((f) => f.name).join(", ") : "",
        badge: schema ? `${schema.fields.length} fields` : "",
        relation: "input",
        itemName: inputSchemaName,
      } satisfies DetailSubNodeData,
    });
    edges.push({
      id: `${schemaId}->central:input`,
      source: schemaId,
      target: DETAIL_PREFIX_CENTRAL,
      type: "referenceEdge",
      label: "input",
      markerEnd: refMarker(SUB_COLORS.schema),
      data: { layerKind: "schemas" },
    });
  }

  if (outputSchemaName) {
    const schema = doc.schemas?.find((s) => s.name === outputSchemaName);
    const schemaId = DETAIL_PREFIX_SCHEMA + outputSchemaName + ":output";
    nodes.push({
      id: schemaId,
      type: "detailSubNode",
      position: { x: 0, y: 0 },
      data: {
        subKind: "schema",
        label: outputSchemaName,
        subtitle: schema ? schema.fields.map((f) => f.name).join(", ") : "",
        badge: schema ? `${schema.fields.length} fields` : "",
        relation: "output",
        itemName: outputSchemaName,
      } satisfies DetailSubNodeData,
    });
    edges.push({
      id: `central->:${schemaId}:output`,
      source: DETAIL_PREFIX_CENTRAL,
      target: schemaId,
      type: "referenceEdge",
      label: "output",
      markerEnd: refMarker(SUB_COLORS.schema),
      data: { layerKind: "schemas" },
    });
  }

  // 3. Prompt sub-nodes
  const systemPromptName = (agent)?.system ?? (router)?.system;
  const userPromptName = (agent)?.user ?? (router)?.user;
  const instructionsName = (human)?.instructions;

  const promptRefs: { name: string; relation: string }[] = [];
  if (systemPromptName) promptRefs.push({ name: systemPromptName, relation: "system" });
  if (userPromptName) promptRefs.push({ name: userPromptName, relation: "user" });
  if (instructionsName) promptRefs.push({ name: instructionsName, relation: "instructions" });

  for (const pRef of promptRefs) {
    const prompt = doc.prompts?.find((p) => p.name === pRef.name);
    const promptId = DETAIL_PREFIX_PROMPT + pRef.name + ":" + pRef.relation;
    const preview = prompt ? (prompt.body.length > 60 ? prompt.body.slice(0, 60) + "..." : prompt.body).replace(/\n/g, " ") : "";
    nodes.push({
      id: promptId,
      type: "detailSubNode",
      position: { x: 0, y: 0 },
      data: {
        subKind: "prompt",
        label: pRef.name,
        subtitle: preview,
        relation: pRef.relation,
        itemName: pRef.name,
      } satisfies DetailSubNodeData,
    });
    edges.push({
      id: `${promptId}->central:${pRef.relation}`,
      source: promptId,
      target: DETAIL_PREFIX_CENTRAL,
      type: "referenceEdge",
      label: pRef.relation,
      markerEnd: refMarker(SUB_COLORS.prompt),
      data: { layerKind: "prompts" },
    });
  }

  // 4. Var sub-nodes (vars referenced in the node's prompts)
  const varsFound = new Map<string, string>(); // varName -> promptNodeId
  for (const pRef of promptRefs) {
    const prompt = doc.prompts?.find((p) => p.name === pRef.name);
    if (!prompt) continue;
    const varPattern = /\{\{vars\.(\w+)\}\}/g;
    let match;
    while ((match = varPattern.exec(prompt.body)) !== null) {
      const varName = match[1]!;
      if (!varsFound.has(varName)) {
        varsFound.set(varName, DETAIL_PREFIX_PROMPT + pRef.name + ":" + pRef.relation);
      }
    }
  }

  for (const [varName, promptNodeId] of varsFound) {
    const varField = doc.vars?.fields?.find((v) => v.name === varName);
    const varId = DETAIL_PREFIX_VAR + varName;
    nodes.push({
      id: varId,
      type: "detailSubNode",
      position: { x: 0, y: 0 },
      data: {
        subKind: "var",
        label: varName,
        subtitle: varField ? `${varField.type}${varField.default?.raw ? ` = ${varField.default.raw}` : ""}` : "",
        itemName: varName,
      } satisfies DetailSubNodeData,
    });
    edges.push({
      id: `${varId}->${promptNodeId}`,
      source: varId,
      target: promptNodeId,
      type: "referenceEdge",
      label: varName,
      markerEnd: refMarker(SUB_COLORS.var),
      data: { layerKind: "vars" },
    });
  }

  // 5. Edge sub-nodes
  const workflow = doc.workflows?.find((w) => w.name === workflowName);
  if (workflow) {
    for (let i = 0; i < (workflow.edges ?? []).length; i++) {
      const edge = workflow.edges[i]!;
      if (edge.from !== nodeId && edge.to !== nodeId) continue;

      const isOutgoing = edge.from === nodeId;
      const remoteName = isOutgoing ? edge.to : edge.from;
      const direction = isOutgoing ? "\u2192" : "\u2190";
      const edgeId = DETAIL_PREFIX_EDGE + workflowName + ":" + i;

      const badges: string[] = [];
      if (edge.when) {
        if (edge.when.expr) {
          badges.push(`expr: ${edge.when.expr.length > 24 ? edge.when.expr.slice(0, 24) + "…" : edge.when.expr}`);
        } else if (edge.when.condition) {
          badges.push(edge.when.negated ? `not ${edge.when.condition}` : edge.when.condition);
        }
      }
      if (edge.loop) badges.push(`loop:${edge.loop.name}(${edge.loop.max_iterations})`);
      if (edge.with && edge.with.length > 0) badges.push(`${edge.with.length} mapping${edge.with.length > 1 ? "s" : ""}`);

      nodes.push({
        id: edgeId,
        type: "detailSubNode",
        position: { x: 0, y: 0 },
        data: {
          subKind: "edge",
          label: `${direction} ${remoteName}`,
          subtitle: badges.join(" | "),
          badge: isOutgoing ? "out" : "in",
          edgeIndex: i,
          workflowName,
          targetNodeId: remoteName,
        } satisfies DetailSubNodeData,
      });

      if (isOutgoing) {
        edges.push({
          id: `central->${edgeId}`,
          source: DETAIL_PREFIX_CENTRAL,
          target: edgeId,
          type: "referenceEdge",
          label: badges.length > 0 ? badges[0] : "",
          markerEnd: refMarker(SUB_COLORS.edge),
          data: { color: SUB_COLORS.edge },
        });
      } else {
        edges.push({
          id: `${edgeId}->central`,
          source: edgeId,
          target: DETAIL_PREFIX_CENTRAL,
          type: "referenceEdge",
          label: badges.length > 0 ? badges[0] : "",
          markerEnd: refMarker(SUB_COLORS.edge),
          data: { color: SUB_COLORS.edge },
        });
      }
    }
  }

  // 6. Tool sub-nodes (for agents/judges)
  const tools = (agent)?.tools ?? [];
  for (const toolName of tools) {
    const toolId = DETAIL_PREFIX_TOOL + toolName;
    nodes.push({
      id: toolId,
      type: "detailSubNode",
      position: { x: 0, y: 0 },
      data: {
        subKind: "tool",
        label: toolName,
        subtitle: "tool",
      } satisfies DetailSubNodeData,
    });
    edges.push({
      id: `central->${toolId}`,
      source: DETAIL_PREFIX_CENTRAL,
      target: toolId,
      type: "referenceEdge",
      label: "tool",
      markerEnd: refMarker(SUB_COLORS.tool),
      data: { color: SUB_COLORS.tool },
    });
  }

  return { nodes, edges };
}
