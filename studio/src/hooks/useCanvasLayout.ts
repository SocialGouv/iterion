import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { Node, NodeChange, EdgeChange, Edge as FlowEdge } from "@xyflow/react";
import { useDocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";
import { useGroupStore } from "@/store/groups";
import { useActiveWorkflow } from "@/hooks/useActiveWorkflow";
import { documentToGraph, getTopologyKey, generateLayerNodes, applyGroups } from "@/lib/documentToGraph";
import { expandSubbots, getSubbotExpansionKey } from "@/lib/subbotGraph";
import { useSubbotDocuments } from "@/hooks/useSubbotDocuments";
import { parseGroups } from "@/lib/groups";
import { generateNodeDetailGraph } from "@/lib/nodeDetailGraph";
import { autoLayout } from "@/lib/autoLayout";
import { computeEdgeHandles } from "@/lib/computeEdgeHandles";
import { DEBOUNCE_EDGE_RECOMPUTE_MS } from "@/lib/constants";

export function useCanvasLayout() {
  const document = useDocumentStore((s) => s.document);
  const currentFilePath = useDocumentStore((s) => s.currentFilePath);
  const layoutDirection = useUIStore((s) => s.layoutDirection);
  const addToast = useUIStore((s) => s.addToast);
  // Rate-limit auto-layout failure toasts: the layout effect re-runs on
  // every topology change, so a persistently-failing layout would
  // otherwise spam the user. One toast per minute is enough signal.
  const lastLayoutErrorRef = useRef(0);
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

  // Child .bot documents of subbot declarations — drives the inline
  // child-workflow frames. No-op (empty map) when the doc has no subbots.
  const subbotDocs = useSubbotDocuments(document, currentFilePath);

  // Compute graph from document — either main workflow or sub-node detail view
  const { nodes: graphNodes, edges: graphEdges } = useMemo(() => {
    if (!document) return { nodes: [], edges: [] };
    if (subNodeViewId && activeWorkflowName) {
      return generateNodeDetailGraph(document, subNodeViewId, activeWorkflowName);
    }
    const compactGraph = documentToGraph(document, activeWorkflowName);
    let base: { nodes: Node[]; edges: FlowEdge[] } = compactGraph;
    if (subbotDocs.size > 0) {
      // A subbot inside a COLLAPSED group must stay compact: applyGroups
      // hides the group's member ids, and an expanded frame's children
      // (prefixed ids) would survive with a dangling parentId. Filter those
      // subbots out of the expansion so they collapse like any other node.
      const inCollapsedGroup = new Set<string>();
      for (const g of groups) {
        if (collapsedGroups.has(g.name)) for (const nid of g.nodeIds) inCollapsedGroup.add(nid);
      }
      const expandable = inCollapsedGroup.size === 0
        ? subbotDocs
        : new Map([...subbotDocs].filter(([name]) => !inCollapsedGroup.has(name)));
      base = expandSubbots(compactGraph, document, expandable);
    }
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
  }, [document, activeWorkflowName, activeLayers, subNodeViewId, groups, collapsedGroups, subbotDocs]);

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
    // Subbot expansion changes the topology (frame + child nodes + rewired
    // edges) without touching the document — the key must reflect it, or
    // ELK never re-runs when a child doc finishes loading.
    const subbotKey = getSubbotExpansionKey(document, subbotDocs);
    const topoKey = document ? getTopologyKey(document, activeWorkflowName) + "|" + layoutDirection + "|" + layerKey + "|" + subViewKey + "|" + groupKey + "|" + subbotKey : "";
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
          // Fall back to unlaid-out positions so the canvas stays usable,
          // but don't fail silently — a broken layout is otherwise invisible.
          layoutNodesRef.current = graphNodes;
          setLayoutNodes(graphNodes);
          setLayoutEdges(computeEdgeHandles(graphNodes, graphEdges, layoutDirection));
          const now = Date.now();
          if (now - lastLayoutErrorRef.current > 60_000) {
            lastLayoutErrorRef.current = now;
            addToast("Canvas auto-layout failed — using fallback positions", "warning");
          }
        });
    }
  }, [document, graphNodes, graphEdges, activeWorkflowName, layoutDirection, activeLayers, subNodeViewId, groups, collapsedGroups, subbotDocs, addToast]);

  // Property-edit patch: when graphNodes content changes without
  // topology change (e.g., a form edit to model / reasoning_effort),
  // the layout-on-topology-change effect above bails out and the
  // canvas keeps stale `data`. Patch each existing layoutNode's
  // `data` from the matching graphNode without disturbing position.
  useEffect(() => {
    if (graphNodes.length === 0) return;
    const byId = new Map(graphNodes.map((n) => [n.id, n]));
    setLayoutNodes((prev) => {
      if (prev.length === 0) return prev;
      let changed = false;
      const next = prev.map((n) => {
        const fresh = byId.get(n.id);
        if (!fresh || fresh.data === n.data) return n;
        changed = true;
        return { ...n, data: fresh.data };
      });
      if (changed) layoutNodesRef.current = next;
      return changed ? next : prev;
    });
  }, [graphNodes]);

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
        .catch(() => {
          // User-initiated (Arrange button): a no-op failure would look
          // like the click did nothing. Surface it.
          addToast("Arrange failed — try again or reload the workflow", "error");
        });
    },
    [graphNodes, graphEdges, layoutDirection, addToast],
  );

  // selectNodes flips the `selected` flag on each layoutNode whose id
  // matches an entry in `ids` (or all editable nodes when ids === null).
  // Used by the Cmd+A handler in useCanvasKeyboard so xyflow's native
  // multi-selection machinery picks the selection up without needing
  // a second selection store.
  const selectNodes = useCallback((ids: string[] | null) => {
    setLayoutNodes((nds) => {
      const target = ids ? new Set(ids) : null;
      return nds.map((n) => {
        const selected = target ? target.has(n.id) : false;
        if (n.selected === selected) return n;
        return { ...n, selected };
      });
    });
  }, []);

  return {
    layoutNodes,
    layoutEdges,
    pendingPositionsRef,
    onNodesChange,
    onEdgesChange,
    handleArrange,
    selectNodes,
  };
}
