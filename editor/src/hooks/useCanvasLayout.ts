import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { Node, NodeChange, EdgeChange, Edge as FlowEdge } from "@xyflow/react";
import { useDocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";
import { useActiveWorkflow } from "@/hooks/useActiveWorkflow";
import { documentToGraph, getTopologyKey, generateLayerNodes } from "@/lib/documentToGraph";
import { generateNodeDetailGraph } from "@/lib/nodeDetailGraph";
import { autoLayout } from "@/lib/autoLayout";
import { computeEdgeHandles } from "@/lib/computeEdgeHandles";
import { DEBOUNCE_EDGE_RECOMPUTE_MS } from "@/lib/constants";

export function useCanvasLayout() {
  const document = useDocumentStore((s) => s.document);
  const layoutDirection = useUIStore((s) => s.layoutDirection);
  const activeLayers = useUIStore((s) => s.activeLayers);
  const subNodeViewStack = useUIStore((s) => s.subNodeViewStack);
  const activeWorkflow = useActiveWorkflow();
  const activeWorkflowName = activeWorkflow?.name;

  // Current sub-node view target (top of stack)
  const subNodeViewId = subNodeViewStack.length > 0 ? subNodeViewStack[subNodeViewStack.length - 1]! : null;

  // Compute graph from document — either main workflow or sub-node detail view
  const { nodes: graphNodes, edges: graphEdges } = useMemo(() => {
    if (!document) return { nodes: [], edges: [] };
    if (subNodeViewId && activeWorkflowName) {
      return generateNodeDetailGraph(document, subNodeViewId, activeWorkflowName);
    }
    const base = documentToGraph(document, activeWorkflowName);
    const layer = generateLayerNodes(document, activeLayers);
    return {
      nodes: [...base.nodes, ...layer.nodes],
      edges: [...base.edges, ...layer.edges],
    };
  }, [document, activeWorkflowName, activeLayers, subNodeViewId]);

  // Manage node positions with local state (allows dragging)
  const [layoutNodes, setLayoutNodes] = useState<Node[]>([]);
  const [layoutEdges, setLayoutEdges] = useState<FlowEdge[]>([]);
  const prevTopologyRef = useRef<string>("");
  const dragRecomputeTimer = useRef<ReturnType<typeof setTimeout>>(undefined);
  const layoutNodesRef = useRef<Node[]>([]);

  // Cleanup debounce timer on unmount
  useEffect(() => () => clearTimeout(dragRecomputeTimer.current), []);

  // Pending drop positions: nodes dropped before layout runs get placed here
  const pendingPositionsRef = useRef<Map<string, { x: number; y: number }>>(new Map());

  // Auto-layout only when topology changes (nodes/edges added/removed), not on property edits
  useEffect(() => {
    if (graphNodes.length === 0) {
      setLayoutNodes([]);
      setLayoutEdges([]);
      prevTopologyRef.current = "";
      return;
    }
    const layerKey = Array.from(activeLayers).sort().join(",");
    const subViewKey = subNodeViewId ?? "";
    const topoKey = document ? getTopologyKey(document, activeWorkflowName) + "|" + layoutDirection + "|" + layerKey + "|" + subViewKey : "";
    if (prevTopologyRef.current !== topoKey) {
      prevTopologyRef.current = topoKey;
      autoLayout(graphNodes, graphEdges, layoutDirection)
        .then((laid) => {
          const pending = pendingPositionsRef.current;
          let resultNodes: Node[];
          if (pending.size > 0) {
            resultNodes = laid.map((n) => {
              const pos = pending.get(n.id);
              return pos ? { ...n, position: pos } : n;
            });
            pending.clear();
          } else {
            resultNodes = laid;
          }
          layoutNodesRef.current = resultNodes;
          setLayoutNodes(resultNodes);
          setLayoutEdges(computeEdgeHandles(resultNodes, graphEdges, layoutDirection));
        })
        .catch(() => {
          layoutNodesRef.current = graphNodes;
          setLayoutNodes(graphNodes);
          setLayoutEdges(computeEdgeHandles(graphNodes, graphEdges, layoutDirection));
        });
    }
  }, [document, graphNodes, graphEdges, activeWorkflowName, layoutDirection, activeLayers, subNodeViewId]);

  const onNodesChange = useCallback(
    (changes: NodeChange[]) => {
      const hasPositionChange = changes.some((c) => c.type === "position" && c.position);
      setLayoutNodes((nds) => {
        const updated = nds.map((n) => {
          const change = changes.find((c) => c.type === "position" && c.id === n.id);
          if (change && change.type === "position" && change.position) {
            return { ...n, position: change.position };
          }
          return n;
        });
        layoutNodesRef.current = updated;
        return updated;
      });
      if (hasPositionChange) {
        clearTimeout(dragRecomputeTimer.current);
        dragRecomputeTimer.current = setTimeout(() => {
          setLayoutEdges(computeEdgeHandles(layoutNodesRef.current, graphEdges, layoutDirection));
        }, DEBOUNCE_EDGE_RECOMPUTE_MS);
      }
    },
    [graphEdges, layoutDirection],
  );

  const onEdgesChange = useCallback((_changes: EdgeChange[]) => {
    // Edge changes handled via document store, not ReactFlow state
  }, []);

  const handleArrange = useCallback(
    (fitView: (opts: { padding: number }) => void) => {
      autoLayout(graphNodes, graphEdges, layoutDirection)
        .then((laid) => {
          layoutNodesRef.current = laid;
          setLayoutNodes(laid);
          setLayoutEdges(computeEdgeHandles(laid, graphEdges, layoutDirection));
          prevTopologyRef.current = "";
          setTimeout(() => fitView({ padding: 0.2 }), 50);
        })
        .catch(() => {});
    },
    [graphNodes, graphEdges, layoutDirection],
  );

  return {
    layoutNodes,
    layoutEdges,
    pendingPositionsRef,
    onNodesChange,
    onEdgesChange,
    handleArrange,
  };
}
