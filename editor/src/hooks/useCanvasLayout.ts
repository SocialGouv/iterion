import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { Node, NodeChange, EdgeChange, Edge as FlowEdge } from "@xyflow/react";
import { useDocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";
import { useGroupStore } from "@/store/groups";
import { useActiveWorkflow } from "@/hooks/useActiveWorkflow";
import { documentToGraph, getTopologyKey, generateLayerNodes, applyGroups } from "@/lib/documentToGraph";
import { parseGroups } from "@/lib/groups";
import { generateNodeDetailGraph } from "@/lib/nodeDetailGraph";
import { autoLayout } from "@/lib/autoLayout";
import { computeEdgeHandles } from "@/lib/computeEdgeHandles";
import { DEBOUNCE_EDGE_RECOMPUTE_MS } from "@/lib/constants";

export function useCanvasLayout() {
  const document = useDocumentStore((s) => s.document);
  const layoutDirection = useUIStore((s) => s.layoutDirection);
  const activeLayers = useUIStore((s) => s.activeLayers);
  const subNodeViewStack = useUIStore((s) => s.subNodeViewStack);
  const macroView = useUIStore((s) => s.macroView);
  const collapsedGroups = useGroupStore((s) => s.collapsedGroups);
  const collapseAll = useGroupStore((s) => s.collapseAll);
  const expandAll = useGroupStore((s) => s.expandAll);
  const activeWorkflow = useActiveWorkflow();
  const activeWorkflowName = activeWorkflow?.name;

  // Current sub-node view target (top of stack)
  const subNodeViewId = subNodeViewStack.length > 0 ? subNodeViewStack[subNodeViewStack.length - 1]! : null;

  // Parse groups from document comments
  const groups = useMemo(() => {
    if (!document) return [];
    return parseGroups(document.comments ?? []);
  }, [document]);

  // Macro view: collapse/expand all groups when toggled
  useEffect(() => {
    if (macroView && groups.length > 0) {
      collapseAll(groups.map((g) => g.name));
    } else if (!macroView) {
      expandAll();
    }
  }, [macroView, groups, collapseAll, expandAll]);

  // Compute graph from document — either main workflow or sub-node detail view
  const { nodes: graphNodes, edges: graphEdges } = useMemo(() => {
    if (!document) return { nodes: [], edges: [] };
    if (subNodeViewId && activeWorkflowName) {
      return generateNodeDetailGraph(document, subNodeViewId, activeWorkflowName);
    }
    const base = documentToGraph(document, activeWorkflowName);
    const layer = generateLayerNodes(document, activeLayers);
    const merged = {
      nodes: [...base.nodes, ...layer.nodes],
      edges: [...base.edges, ...layer.edges],
    };
    // Apply group annotations (no-op if no groups defined)
    if (groups.length > 0 && !subNodeViewId) {
      return applyGroups(merged.nodes, merged.edges, groups, collapsedGroups);
    }
    return merged;
  }, [document, activeWorkflowName, activeLayers, subNodeViewId, groups, collapsedGroups]);

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
    const groupKey = groups.map((g) => `${g.name}:${g.nodeIds.join("+")}:${collapsedGroups.has(g.name) ? "c" : "e"}`).join(";");
    const topoKey = document ? getTopologyKey(document, activeWorkflowName) + "|" + layoutDirection + "|" + layerKey + "|" + subViewKey + "|" + groupKey : "";
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
  }, [document, graphNodes, graphEdges, activeWorkflowName, layoutDirection, activeLayers, subNodeViewId, groups, collapsedGroups]);

  const onNodesChange = useCallback(
    (changes: NodeChange[]) => {
      const hasPositionChange = changes.some((c) => c.type === "position" && c.position);
      setLayoutNodes((nds) => {
        const updated = nds.map((n) => {
          let result = n;
          for (const change of changes) {
            if (!("id" in change) || change.id !== n.id) continue;
            if (change.type === "position" && change.position) {
              result = { ...result, position: change.position };
            } else if (change.type === "select") {
              result = { ...result, selected: change.selected };
            }
          }
          return result;
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
