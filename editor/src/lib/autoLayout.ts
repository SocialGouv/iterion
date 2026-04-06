import ELK from "elkjs/lib/elk.bundled.js";
import type { ElkNode } from "elkjs/lib/elk.bundled.js";
import type { Node, Edge as FlowEdge } from "@xyflow/react";
import { isAuxiliaryNodeId } from "./documentToGraph";
import { isGroupNodeId } from "./groups";

const elk = new ELK();

import { NODE_WIDTH, NODE_HEIGHT, AUX_NODE_WIDTH, AUX_NODE_HEIGHT } from "./constants";

const GROUP_PADDING = 40;
const GROUP_HEADER_HEIGHT = 32;

export async function autoLayout(
  nodes: Node[],
  edges: FlowEdge[],
  direction: "DOWN" | "RIGHT" = "DOWN",
): Promise<Node[]> {
  if (nodes.length === 0) return nodes;

  // Separate nodes into groups and children
  const parentMap = new Map<string, string>(); // childId -> parentId
  const groupChildren = new Map<string, Node[]>(); // groupNodeId -> children
  const topLevelNodes: Node[] = [];

  for (const n of nodes) {
    if (n.parentId) {
      parentMap.set(n.id, n.parentId);
      const siblings = groupChildren.get(n.parentId) ?? [];
      siblings.push(n);
      groupChildren.set(n.parentId, siblings);
    } else {
      topLevelNodes.push(n);
    }
  }

  const baseLayoutOptions: Record<string, string> = {
    "elk.algorithm": "layered",
    "elk.direction": direction,
    "elk.spacing.nodeNode": "80",
    "elk.layered.spacing.nodeNodeBetweenLayers": "100",
    "elk.layered.cycleBreaking.strategy": "DEPTH_FIRST",
    "elk.layered.crossingMinimization.strategy": "LAYER_SWEEP",
    "elk.layered.nodePlacement.strategy": "BRANDES_KOEPF",
  };

  function makeElkNode(n: Node): ElkNode {
    const isAux = isAuxiliaryNodeId(n.id);
    const isGroup = isGroupNodeId(n.id);
    const kind = (n.data as Record<string, unknown>)?.kind as string | undefined;
    const layoutOptions: Record<string, string> = {};

    if (n.id === "__start__" || kind === "start") {
      layoutOptions["elk.layered.layering.layerConstraint"] = "FIRST";
    } else if (kind === "done" || kind === "fail") {
      layoutOptions["elk.layered.layering.layerConstraint"] = "LAST";
    }

    // Group node with children — compound node
    const children = groupChildren.get(n.id);
    if (isGroup && children && children.length > 0) {
      return {
        id: n.id,
        layoutOptions: {
          ...baseLayoutOptions,
          "elk.padding": `[top=${GROUP_HEADER_HEIGHT + GROUP_PADDING},left=${GROUP_PADDING},bottom=${GROUP_PADDING},right=${GROUP_PADDING}]`,
          ...layoutOptions,
        },
        children: children.map(makeElkNode),
        edges: [],
      };
    }

    return {
      id: n.id,
      width: isAux ? AUX_NODE_WIDTH : NODE_WIDTH,
      height: isAux ? AUX_NODE_HEIGHT : NODE_HEIGHT,
      ...(Object.keys(layoutOptions).length > 0 && { layoutOptions }),
    };
  }

  // Build the ELK graph — only top-level nodes, children are nested inside group nodes
  const graph: ElkNode = {
    id: "root",
    layoutOptions: baseLayoutOptions,
    children: topLevelNodes.map(makeElkNode),
    edges: edges.map((e) => {
      const isLoop = !!(e.data as Record<string, unknown>)?.loop;
      const isRef = e.type === "referenceEdge";
      // For edges involving nodes inside groups, ELK needs the full hierarchy path.
      // But since we nested children inside group nodes, ELK resolves IDs within the compound graph.
      return {
        id: e.id,
        sources: [e.source],
        targets: [e.target],
        ...((isLoop || isRef) && { layoutOptions: { "elk.layered.priority.direction": "0" } }),
      };
    }),
  };

  const layout = await elk.layout(graph);

  // Extract positions from ELK result (recursive for compound nodes)
  const posMap = new Map<string, { x: number; y: number }>();
  const sizeMap = new Map<string, { width: number; height: number }>();

  function extractPositions(elkNode: ElkNode) {
    for (const child of elkNode.children ?? []) {
      posMap.set(child.id, { x: child.x ?? 0, y: child.y ?? 0 });
      if (child.width && child.height) {
        sizeMap.set(child.id, { width: child.width, height: child.height });
      }
      // Recurse for nested children (compound groups)
      if (child.children) extractPositions(child);
    }
  }
  extractPositions(layout);

  return nodes.map((n) => {
    const pos = posMap.get(n.id);
    const size = sizeMap.get(n.id);
    return {
      ...n,
      position: pos ?? n.position,
      ...(size && { style: { ...((n.style as Record<string, unknown>) ?? {}), width: size.width, height: size.height } }),
    };
  });
}
