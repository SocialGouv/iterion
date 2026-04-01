import { useCallback, useEffect, useMemo, useRef, useState, type DragEvent, type KeyboardEvent } from "react";
import { ReactFlow, Background, Controls, MiniMap, useReactFlow } from "@xyflow/react";
import type { NodeMouseHandler, EdgeMouseHandler, Connection, Node, NodeChange, EdgeChange } from "@xyflow/react";
import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import { useActiveWorkflow } from "@/hooks/useActiveWorkflow";
import { documentToGraph, getTopologyKey, makeEdgeId } from "@/lib/documentToGraph";
import { autoLayout } from "@/lib/autoLayout";
import { generateUniqueName, getAllNodeNames, defaultAgent, defaultJudge, defaultRouter, defaultJoin, defaultHuman, defaultTool } from "@/lib/defaults";
import type { NodeKind } from "@/api/types";
import WorkflowNode from "./WorkflowNode";
import ConditionalEdge from "./ConditionalEdge";

const nodeTypes = { workflowNode: WorkflowNode };
const edgeTypes = { conditionalEdge: ConditionalEdge };

export default function Canvas() {
  const document = useDocumentStore((s) => s.document);
  const addAgent = useDocumentStore((s) => s.addAgent);
  const addJudge = useDocumentStore((s) => s.addJudge);
  const addRouter = useDocumentStore((s) => s.addRouter);
  const addJoin = useDocumentStore((s) => s.addJoin);
  const addHuman = useDocumentStore((s) => s.addHuman);
  const addTool = useDocumentStore((s) => s.addTool);
  const addEdge = useDocumentStore((s) => s.addEdge);
  const removeNode = useDocumentStore((s) => s.removeNode);
  const removeEdge = useDocumentStore((s) => s.removeEdge);
  const setSelectedNode = useSelectionStore((s) => s.setSelectedNode);
  const setSelectedEdge = useSelectionStore((s) => s.setSelectedEdge);
  const clearSelection = useSelectionStore((s) => s.clearSelection);
  const selectedNodeId = useSelectionStore((s) => s.selectedNodeId);
  const selectedEdgeId = useSelectionStore((s) => s.selectedEdgeId);
  const activeWorkflow = useActiveWorkflow();
  const reactFlowWrapper = useRef<HTMLDivElement>(null);
  const { screenToFlowPosition } = useReactFlow();

  const { nodes: graphNodes, edges: graphEdges } = useMemo(() => {
    if (!document) return { nodes: [], edges: [] };
    return documentToGraph(document);
  }, [document]);

  // Manage node positions with local state (allows dragging)
  const [layoutNodes, setLayoutNodes] = useState<Node[]>([]);
  const prevTopologyRef = useRef<string>("");

  // Pending drop positions: nodes dropped before layout runs get placed here
  const pendingPositionsRef = useRef<Map<string, { x: number; y: number }>>(new Map());

  // Auto-layout only when topology changes (nodes/edges added/removed), not on property edits
  useEffect(() => {
    if (graphNodes.length === 0) {
      setLayoutNodes([]);
      prevTopologyRef.current = "";
      return;
    }
    const topoKey = document ? getTopologyKey(document) : "";
    if (prevTopologyRef.current !== topoKey) {
      prevTopologyRef.current = topoKey;
      autoLayout(graphNodes, graphEdges)
        .then((laid) => {
          // Apply any pending drop positions
          const pending = pendingPositionsRef.current;
          if (pending.size > 0) {
            const result = laid.map((n) => {
              const pos = pending.get(n.id);
              return pos ? { ...n, position: pos } : n;
            });
            pending.clear();
            setLayoutNodes(result);
          } else {
            setLayoutNodes(laid);
          }
        })
        .catch(() => setLayoutNodes(graphNodes));
    }
  }, [document, graphNodes, graphEdges]);

  const onNodesChange = useCallback(
    (changes: NodeChange[]) => {
      setLayoutNodes((nds) =>
        nds.map((n) => {
          const change = changes.find((c) => c.type === "position" && c.id === n.id);
          if (change && change.type === "position" && change.position) {
            return { ...n, position: change.position };
          }
          return n;
        }),
      );
    },
    [],
  );

  const onEdgesChange = useCallback((_changes: EdgeChange[]) => {
    // Edge changes handled via document store, not ReactFlow state
  }, []);

  const onNodeClick: NodeMouseHandler = useCallback(
    (_event, node) => {
      setSelectedNode(node.id);
    },
    [setSelectedNode],
  );

  const onEdgeClick: EdgeMouseHandler = useCallback(
    (_event, edge) => {
      setSelectedEdge(edge.id);
    },
    [setSelectedEdge],
  );

  const onPaneClick = useCallback(() => {
    clearSelection();
  }, [clearSelection]);

  const onConnect = useCallback(
    (connection: Connection) => {
      if (!document || !connection.source || !connection.target) return;
      const workflowName = activeWorkflow?.name;
      if (!workflowName) return;
      addEdge(workflowName, { from: connection.source, to: connection.target });
    },
    [document, activeWorkflow, addEdge],
  );

  const onDragOver = useCallback((e: DragEvent) => {
    e.preventDefault();
    e.dataTransfer.dropEffect = "move";
  }, []);

  const onDrop = useCallback(
    (e: DragEvent) => {
      e.preventDefault();
      const kind = e.dataTransfer.getData("application/iterion-node") as NodeKind;
      if (!kind || !document) return;

      // done/fail are virtual terminal nodes, not draggable
      if (kind === "done" || kind === "fail") return;

      const existingNames = getAllNodeNames(document);
      const name = generateUniqueName(kind, existingNames);

      // Store the drop position so the next layout applies it
      const position = screenToFlowPosition({ x: e.clientX, y: e.clientY });
      pendingPositionsRef.current.set(name, position);

      switch (kind) {
        case "agent": addAgent(defaultAgent(name)); break;
        case "judge": addJudge(defaultJudge(name)); break;
        case "router": addRouter(defaultRouter(name)); break;
        case "join": addJoin(defaultJoin(name)); break;
        case "human": addHuman(defaultHuman(name)); break;
        case "tool": addTool(defaultTool(name)); break;
      }
      setSelectedNode(name);
    },
    [document, addAgent, addJudge, addRouter, addJoin, addHuman, addTool, setSelectedNode, screenToFlowPosition],
  );

  const onKeyDown = useCallback(
    (e: KeyboardEvent) => {
      if (e.key === "Delete" || e.key === "Backspace") {
        // Don't handle if user is typing in an input
        if ((e.target as HTMLElement).tagName === "INPUT" || (e.target as HTMLElement).tagName === "TEXTAREA") return;

        if (selectedNodeId && selectedNodeId !== "done" && selectedNodeId !== "fail") {
          removeNode(selectedNodeId);
          clearSelection();
        } else if (selectedEdgeId && document) {
          for (const wf of document.workflows) {
            const wfEdges = wf.edges ?? [];
            for (let i = 0; i < wfEdges.length; i++) {
              const e = wfEdges[i]!;
              const id = makeEdgeId(e.from, e.to, e.when?.condition ?? "", e.when?.negated ?? false, i);
              if (id === selectedEdgeId) {
                removeEdge(wf.name, i);
                clearSelection();
                return;
              }
            }
          }
        }
      }
    },
    [selectedNodeId, selectedEdgeId, document, removeNode, removeEdge, clearSelection],
  );

  return (
    <div className="h-full w-full" ref={reactFlowWrapper} onKeyDown={onKeyDown} tabIndex={0}>
      <ReactFlow
        nodes={layoutNodes}
        edges={graphEdges}
        nodeTypes={nodeTypes}
        edgeTypes={edgeTypes}
        onNodesChange={onNodesChange}
        onEdgesChange={onEdgesChange}
        onNodeClick={onNodeClick}
        onEdgeClick={onEdgeClick}
        onPaneClick={onPaneClick}
        onConnect={onConnect}
        onDragOver={onDragOver}
        onDrop={onDrop}
        fitView
        colorMode="dark"
      >
        <Background />
        <Controls />
        <MiniMap />
      </ReactFlow>
    </div>
  );
}
