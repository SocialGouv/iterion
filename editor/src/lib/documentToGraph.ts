import { MarkerType } from "@xyflow/react";
import type { Node, Edge as FlowEdge } from "@xyflow/react";
import type { IterDocument, NodeKind, AgentDecl, JudgeDecl, HumanDecl, ToolNodeDecl, ComputeDecl } from "@/api/types";
import type { LayerKind } from "@/store/ui";
import type { AuxiliaryNodeData } from "@/components/Canvas/AuxiliaryNode";
import type { GroupNodeData } from "@/components/Canvas/GroupNode";
import type { GroupAnnotation } from "./groups";
import { makeGroupNodeId } from "./groups";
import { NODE_COLORS, LAYER_COLORS } from "./constants";

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
    (doc.humans ?? []).length,
    (doc.tools ?? []).length,
    (doc.computes ?? []).length,
  ].join(",");
  const targetWorkflows = activeWorkflowName
    ? (doc.workflows ?? []).filter(w => w.name === activeWorkflowName)
    : doc.workflows ?? [];
  const edgeSigs: string[] = [];
  let entry = "";
  for (const wf of targetWorkflows) {
    for (const e of wf.edges ?? []) edgeSigs.push(`${e.from}->${e.to}`);
    if (wf.entry) entry = wf.entry;
  }
  return `${activeWorkflowName ?? ""}|${counts}|${entry}|${edgeSigs.join(",")}`;
}

export function documentToGraph(doc: IterDocument, activeWorkflowName?: string): { nodes: Node<NodeData>[]; edges: FlowEdge[] } {
  const nodeMap = new Map<string, { kind: NodeKind; decl: unknown }>();

  for (const a of doc.agents ?? []) nodeMap.set(a.name, { kind: "agent", decl: a });
  for (const j of doc.judges ?? []) nodeMap.set(j.name, { kind: "judge", decl: j });
  for (const r of doc.routers ?? []) nodeMap.set(r.name, { kind: "router", decl: r });
  for (const h of doc.humans ?? []) nodeMap.set(h.name, { kind: "human", decl: h });
  for (const t of doc.tools ?? []) nodeMap.set(t.name, { kind: "tool", decl: t });
  for (const c of doc.computes ?? []) nodeMap.set(c.name, { kind: "compute", decl: c });

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

  // Add virtual start node pointing to the workflow entry
  const entryNode = targetWorkflows.length > 0 ? targetWorkflows[0]!.entry : undefined;
  if (entryNode && nodeMap.has(entryNode)) {
    nodeMap.set("__start__", { kind: "start", decl: null });
  }

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
        // expression form takes precedence over the simple boolean field;
        // the runtime evaluates Expr when set, ignoring Condition/Negated.
        if (edge.when.expr) {
          label = edge.when.expr.length > 24 ? `${edge.when.expr.slice(0, 24)}…` : edge.when.expr;
        } else if (edge.when.condition) {
          label = edge.when.negated ? `!${edge.when.condition}` : edge.when.condition;
        }
      }
      if (edge.loop) {
        label += `${label ? " " : ""}loop:${edge.loop.name}(${edge.loop.max_iterations})`;
      }
      if (edge.with && edge.with.length > 0) {
        label += `${label ? " " : ""}[${edge.with.length} mapping${edge.with.length > 1 ? "s" : ""}]`;
      }
      const isLoop = !!edge.loop;
      edges.push({
        id: makeEdgeId(wf.name, i),
        source: edge.from,
        target: edge.to,
        type: "conditionalEdge",
        label: label || undefined,
        markerEnd: { type: MarkerType.ArrowClosed, color: isLoop ? "#F59E0B" : "#888", width: 16, height: 16 },
        data: { when: edge.when, loop: edge.loop, with: edge.with, edgeIndex: i, workflowName: wf.name },
      });
    }
  }

  // Add edge from virtual start node to the entry node
  if (entryNode && nodeMap.has("__start__")) {
    edges.push({
      id: "__start__:entry",
      source: "__start__",
      target: entryNode,
      type: "default",
      markerEnd: { type: MarkerType.ArrowClosed, color: "#888", width: 16, height: 16 },
    });
  }

  return { nodes, edges };
}

const GROUP_COLOR = "#6366F1";

/** Apply group annotations to a graph: create group container nodes and handle collapsed groups.
 *  - Expanded groups: group node added, children get parentId (for ELK compound layout)
 *  - Collapsed groups: children hidden, edges aggregated to point to/from the group node */
export function applyGroups(
  nodes: Node[],
  edges: FlowEdge[],
  groups: GroupAnnotation[],
  collapsedGroups: Set<string>,
): { nodes: Node[]; edges: FlowEdge[] } {
  if (groups.length === 0) return { nodes, edges };

  // Build lookups
  const nodeToGroup = new Map<string, string>();
  for (const g of groups) {
    for (const nid of g.nodeIds) {
      nodeToGroup.set(nid, g.name);
    }
  }
  const nodeById = new Map<string, Node>();
  for (const n of nodes) nodeById.set(n.id, n);

  const resultNodes: Node[] = [];
  const resultEdges: FlowEdge[] = [];
  const hiddenNodeIds = new Set<string>();

  // Determine which nodes are hidden (in collapsed groups)
  for (const g of groups) {
    if (collapsedGroups.has(g.name)) {
      for (const nid of g.nodeIds) hiddenNodeIds.add(nid);
    }
  }

  // Add group nodes
  for (const g of groups) {
    const groupNodeId = makeGroupNodeId(g.name);
    const isCollapsed = collapsedGroups.has(g.name);

    // Collect kinds of children for summary
    const childKindSet = new Set<string>();
    for (const nid of g.nodeIds) {
      const found = nodeById.get(nid);
      if (found) {
        const kind = (found.data as Record<string, unknown>)?.kind as string | undefined;
        if (kind) childKindSet.add(kind);
      }
    }

    if (isCollapsed) {
      // Collapsed: add a compact group node (regular size, handles for edges)
      resultNodes.push({
        id: groupNodeId,
        type: "groupNode",
        position: { x: 0, y: 0 },
        data: {
          groupName: g.name,
          nodeCount: g.nodeIds.length,
          childKinds: Array.from(childKindSet),
          color: GROUP_COLOR,
        } as GroupNodeData,
      });
    } else {
      // Expanded: add a container group node — children will be placed inside by ELK
      resultNodes.push({
        id: groupNodeId,
        type: "groupNode",
        position: { x: 0, y: 0 },
        style: { width: 400, height: 300 },
        data: {
          groupName: g.name,
          nodeCount: g.nodeIds.length,
          childKinds: Array.from(childKindSet),
          color: GROUP_COLOR,
        } as GroupNodeData,
      });
    }
  }

  // Add visible nodes (not in collapsed groups), with parentId for expanded groups
  for (const node of nodes) {
    if (hiddenNodeIds.has(node.id)) continue;
    const groupName = nodeToGroup.get(node.id);
    if (groupName && !collapsedGroups.has(groupName)) {
      // Node is in an expanded group: set parentId
      resultNodes.push({
        ...node,
        parentId: makeGroupNodeId(groupName),
        extent: "parent" as const,
      });
    } else {
      resultNodes.push(node);
    }
  }

  // Process edges: aggregate for collapsed groups
  const seenAggregated = new Set<string>();
  for (const edge of edges) {
    const srcGroup = nodeToGroup.get(edge.source);
    const tgtGroup = nodeToGroup.get(edge.target);
    const srcCollapsed = srcGroup && collapsedGroups.has(srcGroup);
    const tgtCollapsed = tgtGroup && collapsedGroups.has(tgtGroup);

    if (!srcCollapsed && !tgtCollapsed) {
      // Neither end is in a collapsed group — keep as-is
      resultEdges.push(edge);
      continue;
    }

    // At least one end is in a collapsed group — aggregate
    const newSource = srcCollapsed ? makeGroupNodeId(srcGroup) : edge.source;
    const newTarget = tgtCollapsed ? makeGroupNodeId(tgtGroup) : edge.target;

    // Skip internal edges (both ends in same collapsed group)
    if (newSource === newTarget) continue;

    // Dedup aggregated edges
    const aggKey = `${newSource}->${newTarget}`;
    if (seenAggregated.has(aggKey)) continue;
    seenAggregated.add(aggKey);

    // Preserve edge annotations from the first matching edge
    resultEdges.push({
      ...edge,
      id: `agg:${aggKey}`,
      source: newSource,
      target: newTarget,
    });
  }

  return { nodes: resultNodes, edges: resultEdges };
}

// Prefixes for auxiliary node IDs
export const AUX_PREFIX_SCHEMA = "__schema__:";
export const AUX_PREFIX_PROMPT = "__prompt__:";
export const AUX_PREFIX_VAR = "__var__:";

export function isAuxiliaryNodeId(id: string): boolean {
  return id.startsWith(AUX_PREFIX_SCHEMA) || id.startsWith(AUX_PREFIX_PROMPT) || id.startsWith(AUX_PREFIX_VAR);
}

/** Generate overlay layer nodes and reference edges from the document */
function refMarker(layerKind: LayerKind) {
  return { type: MarkerType.ArrowClosed as const, color: LAYER_COLORS[layerKind], width: 12, height: 12 };
}

export function generateLayerNodes(
  doc: IterDocument,
  activeLayers: Set<LayerKind>,
): { nodes: Node<AuxiliaryNodeData>[]; edges: FlowEdge[] } {
  const nodes: Node<AuxiliaryNodeData>[] = [];
  const edges: FlowEdge[] = [];

  if (activeLayers.size === 0) return { nodes, edges };

  // Collect all workflow node declarations with their schema/prompt references
  const allDecls: { name: string; input?: string; output?: string; system?: string; user?: string; instructions?: string }[] = [];
  for (const a of doc.agents ?? []) allDecls.push({ name: a.name, input: (a as AgentDecl).input, output: (a as AgentDecl).output, system: (a as AgentDecl).system, user: (a as AgentDecl).user });
  for (const j of doc.judges ?? []) allDecls.push({ name: j.name, input: (j as JudgeDecl).input, output: (j as JudgeDecl).output, system: (j as JudgeDecl).system, user: (j as JudgeDecl).user });
  for (const h of doc.humans ?? []) allDecls.push({ name: h.name, input: (h as HumanDecl).input, output: (h as HumanDecl).output, instructions: (h as HumanDecl).instructions });
  for (const t of doc.tools ?? []) allDecls.push({ name: t.name, input: (t as ToolNodeDecl).input, output: (t as ToolNodeDecl).output });
  for (const c of doc.computes ?? []) allDecls.push({ name: c.name, input: (c as ComputeDecl).input, output: (c as ComputeDecl).output });

  // --- Schemas layer ---
  if (activeLayers.has("schemas")) {
    for (const schema of doc.schemas ?? []) {
      const nodeId = AUX_PREFIX_SCHEMA + schema.name;
      nodes.push({
        id: nodeId,
        type: "auxiliaryNode",
        position: { x: 0, y: 0 },
        draggable: false,
        data: {
          label: schema.name,
          layerKind: "schemas",
          subtitle: schema.fields.map((f) => f.name).join(", "),
          badge: `${schema.fields.length}`,
        },
      });
      // Connect to workflow nodes that reference this schema
      for (const decl of allDecls) {
        if (decl.input === schema.name) {
          edges.push({
            id: `${nodeId}->ref:${decl.name}:input`,
            source: nodeId,
            target: decl.name,
            type: "referenceEdge",
            label: "input",
            markerEnd: refMarker("schemas"),
            data: { layerKind: "schemas" },
          });
        }
        if (decl.output === schema.name) {
          edges.push({
            id: `${nodeId}->ref:${decl.name}:output`,
            source: decl.name,
            target: nodeId,
            type: "referenceEdge",
            label: "output",
            markerEnd: refMarker("schemas"),
            data: { layerKind: "schemas" },
          });
        }
      }
    }
  }

  // --- Prompts layer ---
  if (activeLayers.has("prompts")) {
    for (const prompt of doc.prompts ?? []) {
      const nodeId = AUX_PREFIX_PROMPT + prompt.name;
      const preview = prompt.body.length > 40 ? prompt.body.slice(0, 40) + "..." : prompt.body;
      nodes.push({
        id: nodeId,
        type: "auxiliaryNode",
        position: { x: 0, y: 0 },
        draggable: false,
        data: {
          label: prompt.name,
          layerKind: "prompts",
          subtitle: preview.replace(/\n/g, " "),
        },
      });
      for (const decl of allDecls) {
        if (decl.system === prompt.name) {
          edges.push({
            id: `${nodeId}->ref:${decl.name}:system`,
            source: nodeId,
            target: decl.name,
            type: "referenceEdge",
            label: "system",
            markerEnd: refMarker("prompts"),
            data: { layerKind: "prompts" },
          });
        }
        if (decl.user === prompt.name) {
          edges.push({
            id: `${nodeId}->ref:${decl.name}:user`,
            source: nodeId,
            target: decl.name,
            type: "referenceEdge",
            label: "user",
            markerEnd: refMarker("prompts"),
            data: { layerKind: "prompts" },
          });
        }
        if (decl.instructions === prompt.name) {
          edges.push({
            id: `${nodeId}->ref:${decl.name}:instructions`,
            source: nodeId,
            target: decl.name,
            type: "referenceEdge",
            label: "instructions",
            markerEnd: refMarker("prompts"),
            data: { layerKind: "prompts" },
          });
        }
      }
    }
  }

  // --- Vars layer ---
  if (activeLayers.has("vars")) {
    const varFields = doc.vars?.fields ?? [];
    // Build a map: prompt name -> set of workflow nodes using it
    const promptToNodes = new Map<string, string[]>();
    for (const decl of allDecls) {
      if (decl.system) promptToNodes.set(decl.system, [...(promptToNodes.get(decl.system) ?? []), decl.name]);
      if (decl.user) promptToNodes.set(decl.user, [...(promptToNodes.get(decl.user) ?? []), decl.name]);
      if (decl.instructions) promptToNodes.set(decl.instructions, [...(promptToNodes.get(decl.instructions) ?? []), decl.name]);
    }

    for (const v of varFields) {
      const nodeId = AUX_PREFIX_VAR + v.name;
      const defaultStr = v.default?.raw ? `= ${v.default.raw}` : "";
      nodes.push({
        id: nodeId,
        type: "auxiliaryNode",
        position: { x: 0, y: 0 },
        draggable: false,
        data: {
          label: v.name,
          layerKind: "vars",
          subtitle: `${v.type} ${defaultStr}`.trim(),
        },
      });

      // Find which prompts reference this var via {{vars.NAME}}
      const pattern = `{{vars.${v.name}}}`;
      for (const prompt of doc.prompts ?? []) {
        if (prompt.body.includes(pattern)) {
          const promptNodeId = AUX_PREFIX_PROMPT + prompt.name;
          // If prompts layer is active, link to prompt node
          if (activeLayers.has("prompts")) {
            edges.push({
              id: `${nodeId}->ref:${promptNodeId}`,
              source: nodeId,
              target: promptNodeId,
              type: "referenceEdge",
              label: v.name,
              markerEnd: refMarker("vars"),
              data: { layerKind: "vars" },
            });
          } else {
            // Link directly to workflow nodes using this prompt
            const wfNodes = promptToNodes.get(prompt.name) ?? [];
            for (const wfNode of wfNodes) {
              edges.push({
                id: `${nodeId}->ref:${wfNode}:via:${prompt.name}`,
                source: nodeId,
                target: wfNode,
                type: "referenceEdge",
                label: v.name,
                data: { layerKind: "vars" },
              });
            }
          }
        }
      }
    }
  }

  return { nodes, edges };
}
