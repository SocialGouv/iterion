import ELK from "elkjs/lib/elk.bundled.js";
import type { Node, Edge as FlowEdge } from "@xyflow/react";
import { isAuxiliaryNodeId } from "./documentToGraph";

const elk = new ELK();

const NODE_WIDTH = 160;
const NODE_HEIGHT = 80;
const AUX_NODE_WIDTH = 120;
const AUX_NODE_HEIGHT = 44;

export async function autoLayout(
  nodes: Node[],
  edges: FlowEdge[],
  direction: "DOWN" | "RIGHT" = "DOWN",
): Promise<Node[]> {
  if (nodes.length === 0) return nodes;

  const graph = {
    id: "root",
    layoutOptions: {
      "elk.algorithm": "layered",
      "elk.direction": direction,
      "elk.spacing.nodeNode": "80",
      "elk.layered.spacing.nodeNodeBetweenLayers": "100",
      "elk.layered.cycleBreaking.strategy": "DEPTH_FIRST",
      "elk.layered.crossingMinimization.strategy": "LAYER_SWEEP",
      "elk.layered.nodePlacement.strategy": "BRANDES_KOEPF",
    },
    children: nodes.map((n) => {
      const isAux = isAuxiliaryNodeId(n.id);
      const kind = (n.data as Record<string, unknown>)?.kind as string | undefined;
      const layoutOptions: Record<string, string> = {};
      if (n.id === "__start__" || kind === "start") {
        layoutOptions["elk.layered.layering.layerConstraint"] = "FIRST";
      } else if (kind === "done" || kind === "fail") {
        layoutOptions["elk.layered.layering.layerConstraint"] = "LAST";
      }
      return {
        id: n.id,
        width: isAux ? AUX_NODE_WIDTH : NODE_WIDTH,
        height: isAux ? AUX_NODE_HEIGHT : NODE_HEIGHT,
        ...(Object.keys(layoutOptions).length > 0 && { layoutOptions }),
      };
    }),
    edges: edges.map((e) => {
      const isLoop = !!(e.data as Record<string, unknown>)?.loop;
      const isRef = e.type === "referenceEdge";
      return {
        id: e.id,
        sources: [e.source],
        targets: [e.target],
        ...((isLoop || isRef) && { layoutOptions: { "elk.layered.priority.direction": "0" } }),
      };
    }),
  };

  const layout = await elk.layout(graph);
  const posMap = new Map<string, { x: number; y: number }>();
  for (const child of layout.children ?? []) {
    posMap.set(child.id, { x: child.x ?? 0, y: child.y ?? 0 });
  }

  return nodes.map((n) => ({
    ...n,
    position: posMap.get(n.id) ?? n.position,
  }));
}
