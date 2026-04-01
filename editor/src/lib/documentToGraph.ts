import type { Node, Edge as FlowEdge } from "@xyflow/react";
import type { IterDocument, NodeKind } from "@/api/types";

export const NODE_COLORS: Record<NodeKind, string> = {
  agent: "#4A90D9",
  judge: "#7B68EE",
  router: "#E67E22",
  join: "#2ECC71",
  human: "#E74C3C",
  tool: "#8B6914",
  done: "#2ECC71",
  fail: "#E74C3C",
};

export interface NodeData extends Record<string, unknown> {
  label: string;
  kind: NodeKind;
  color: string;
  decl: unknown;
}

export function makeEdgeId(workflowName: string, index: number): string {
  return `${workflowName}:edge:${index}`;
}

/** Returns a key that changes only when the graph topology changes (nodes added/removed, edges added/removed).
 *  Uses counts and edge signatures instead of node names, so renaming a node does not trigger relayout. */
export function getTopologyKey(doc: IterDocument, activeWorkflowName?: string): string {
  const counts = [
    (doc.agents ?? []).length,
    (doc.judges ?? []).length,
    (doc.routers ?? []).length,
    (doc.joins ?? []).length,
    (doc.humans ?? []).length,
    (doc.tools ?? []).length,
  ].join(",");
  const targetWorkflows = activeWorkflowName
    ? (doc.workflows ?? []).filter(w => w.name === activeWorkflowName)
    : doc.workflows ?? [];
  const edgeSigs: string[] = [];
  for (const wf of targetWorkflows) {
    for (const e of wf.edges ?? []) edgeSigs.push(`${e.from}->${e.to}`);
  }
  return `${activeWorkflowName ?? ""}|${counts}|${edgeSigs.join(",")}`;
}

export function documentToGraph(doc: IterDocument, activeWorkflowName?: string): { nodes: Node<NodeData>[]; edges: FlowEdge[] } {
  const nodeMap = new Map<string, { kind: NodeKind; decl: unknown }>();

  for (const a of doc.agents ?? []) nodeMap.set(a.name, { kind: "agent", decl: a });
  for (const j of doc.judges ?? []) nodeMap.set(j.name, { kind: "judge", decl: j });
  for (const r of doc.routers ?? []) nodeMap.set(r.name, { kind: "router", decl: r });
  for (const j of doc.joins ?? []) nodeMap.set(j.name, { kind: "join", decl: j });
  for (const h of doc.humans ?? []) nodeMap.set(h.name, { kind: "human", decl: h });
  for (const t of doc.tools ?? []) nodeMap.set(t.name, { kind: "tool", decl: t });

  // Resolve target workflows early so we can check edge references
  const targetWorkflows = activeWorkflowName
    ? (doc.workflows ?? []).filter(w => w.name === activeWorkflowName)
    : doc.workflows ?? [];

  // Only show done/fail terminal nodes when actually referenced by an edge
  const referencedNodes = new Set<string>();
  for (const wf of targetWorkflows) {
    for (const e of wf.edges ?? []) {
      referencedNodes.add(e.from);
      referencedNodes.add(e.to);
    }
  }
  if (!nodeMap.has("done") && referencedNodes.has("done")) nodeMap.set("done", { kind: "done", decl: null });
  if (!nodeMap.has("fail") && referencedNodes.has("fail")) nodeMap.set("fail", { kind: "fail", decl: null });

  // Position nodes in a grid
  const COLS = 4;
  const X_GAP = 250;
  const Y_GAP = 150;
  const names = Array.from(nodeMap.keys());

  const nodes: Node<NodeData>[] = names.map((name, i) => {
    const entry = nodeMap.get(name)!;
    return {
      id: name,
      type: "workflowNode",
      position: {
        x: (i % COLS) * X_GAP + 50,
        y: Math.floor(i / COLS) * Y_GAP + 50,
      },
      // initialWidth/initialHeight ensure the MiniMap can render nodes before DOM measurement
      initialWidth: 140,
      initialHeight: 60,
      data: {
        label: name,
        kind: entry.kind,
        color: NODE_COLORS[entry.kind],
        decl: entry.decl,
      },
    };
  });

  const edges: FlowEdge[] = [];
  for (const wf of targetWorkflows) {
    const wfEdges = wf.edges ?? [];
    for (let i = 0; i < wfEdges.length; i++) {
      const edge = wfEdges[i]!;
      let label = "";
      if (edge.when) {
        label = edge.when.negated ? `!${edge.when.condition}` : edge.when.condition;
      }
      if (edge.loop) {
        label += `${label ? " " : ""}loop:${edge.loop.name}(${edge.loop.max_iterations})`;
      }
      if (edge.with && edge.with.length > 0) {
        label += `${label ? " " : ""}[${edge.with.length} mapping${edge.with.length > 1 ? "s" : ""}]`;
      }
      edges.push({
        id: makeEdgeId(wf.name, i),
        source: edge.from,
        target: edge.to,
        type: label ? "conditionalEdge" : "default",
        label: label || undefined,
        data: { when: edge.when, loop: edge.loop, with: edge.with, edgeIndex: i, workflowName: wf.name },
      });
    }
  }

  return { nodes, edges };
}
