import { useCallback, useEffect, useMemo, useRef, useState, type DragEvent, type KeyboardEvent, type MouseEvent as ReactMouseEvent } from "react";
import { ReactFlow, Background, Controls, MiniMap, useReactFlow } from "@xyflow/react";
import type { NodeMouseHandler, EdgeMouseHandler, Connection, Node, NodeChange, EdgeChange, Edge as FlowEdge } from "@xyflow/react";
import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import { useActiveWorkflow } from "@/hooks/useActiveWorkflow";
import { documentToGraph, getTopologyKey, makeEdgeId } from "@/lib/documentToGraph";
import { autoLayout } from "@/lib/autoLayout";
import { generateUniqueName, getAllNodeNames, defaultAgent, defaultJudge, defaultRouter, defaultJoin, defaultHuman, defaultTool } from "@/lib/defaults";
import type { NodeKind } from "@/api/types";
import WorkflowNode from "./WorkflowNode";
import ConditionalEdge from "./ConditionalEdge";
import NodeContextMenu from "./NodeContextMenu";

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
  const duplicateNode = useDocumentStore((s) => s.duplicateNode);
  const updateWorkflow = useDocumentStore((s) => s.updateWorkflow);
  const setSelectedNode = useSelectionStore((s) => s.setSelectedNode);
  const setSelectedEdge = useSelectionStore((s) => s.setSelectedEdge);
  const clearSelection = useSelectionStore((s) => s.clearSelection);
  const selectedNodeId = useSelectionStore((s) => s.selectedNodeId);
  const selectedEdgeId = useSelectionStore((s) => s.selectedEdgeId);
  const activeWorkflow = useActiveWorkflow();
  const reactFlowWrapper = useRef<HTMLDivElement>(null);
  const { screenToFlowPosition } = useReactFlow();

  // Context menu state
  const [contextMenu, setContextMenu] = useState<{ x: number; y: number; nodeId: string } | null>(null);

  // Search state
  const [searchQuery, setSearchQuery] = useState("");
  const [searchOpen, setSearchOpen] = useState(false);
  const searchInputRef = useRef<HTMLInputElement>(null);

  const activeWorkflowName = activeWorkflow?.name;
  const { nodes: graphNodes, edges: graphEdges } = useMemo(() => {
    if (!document) return { nodes: [], edges: [] };
    return documentToGraph(document, activeWorkflowName);
  }, [document, activeWorkflowName]);

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
    const topoKey = document ? getTopologyKey(document, activeWorkflowName) : "";
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
  }, [document, graphNodes, graphEdges, activeWorkflowName]);

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
    setContextMenu(null);
  }, [clearSelection]);

  const onNodeContextMenu = useCallback(
    (event: ReactMouseEvent, node: Node) => {
      event.preventDefault();
      setContextMenu({ x: event.clientX, y: event.clientY, nodeId: node.id });
    },
    [],
  );

  const isValidConnection = useCallback(
    (connection: FlowEdge | Connection) => {
      if (!connection.source || !connection.target) return false;
      if (connection.source === connection.target) return false;
      if (connection.source === "done" || connection.source === "fail") return false;
      if (activeWorkflow) {
        const dup = activeWorkflow.edges.some(
          (e) => e.from === connection.source && e.to === connection.target,
        );
        if (dup) return false;
      }
      return true;
    },
    [activeWorkflow],
  );

  const onConnect = useCallback(
    (connection: Connection) => {
      if (!document || !connection.source || !connection.target) return;
      const workflowName = activeWorkflow?.name;
      if (!workflowName) return;
      if (!isValidConnection(connection)) return;
      addEdge(workflowName, { from: connection.source, to: connection.target });
    },
    [document, activeWorkflow, addEdge, isValidConnection],
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
      const isInput = (e.target as HTMLElement).matches("input, textarea, select");

      if (e.key === "/" && !isInput) {
        e.preventDefault();
        setSearchOpen(true);
        setTimeout(() => searchInputRef.current?.focus(), 0);
        return;
      }
      if (e.key === "Escape") {
        if (searchOpen) { setSearchOpen(false); setSearchQuery(""); return; }
        clearSelection();
        setContextMenu(null);
        return;
      }
      if (e.key === "Delete" || e.key === "Backspace") {
        if (isInput) return;

        if (selectedNodeId && selectedNodeId !== "done" && selectedNodeId !== "fail") {
          removeNode(selectedNodeId);
          clearSelection();
        } else if (selectedEdgeId && document) {
          for (const wf of document.workflows) {
            const wfEdges = wf.edges ?? [];
            for (let i = 0; i < wfEdges.length; i++) {
              const id = makeEdgeId(wf.name, i);
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
    [selectedNodeId, selectedEdgeId, document, removeNode, removeEdge, clearSelection, searchOpen],
  );

  // Apply search filter: dim non-matching nodes
  const displayNodes = useMemo(() => {
    if (!searchOpen || !searchQuery.trim()) return layoutNodes;
    const q = searchQuery.trim().toLowerCase();
    return layoutNodes.map((n) => {
      const data = n.data as { label: string; kind: string } | undefined;
      const matches =
        n.id.toLowerCase().includes(q) ||
        (data?.label ?? "").toLowerCase().includes(q) ||
        (data?.kind ?? "").toLowerCase().includes(q);
      return matches ? n : { ...n, style: { ...n.style, opacity: 0.25 } };
    });
  }, [layoutNodes, searchOpen, searchQuery]);

  const handleSearchKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      if (e.key === "Escape") {
        setSearchOpen(false);
        setSearchQuery("");
      } else if (e.key === "Enter") {
        // Select first matching node
        const q = searchQuery.trim().toLowerCase();
        if (q) {
          const match = layoutNodes.find((n) => {
            const data = n.data as { label: string; kind: string } | undefined;
            return (
              n.id.toLowerCase().includes(q) ||
              (data?.label ?? "").toLowerCase().includes(q) ||
              (data?.kind ?? "").toLowerCase().includes(q)
            );
          });
          if (match) {
            setSelectedNode(match.id);
            setSearchOpen(false);
            setSearchQuery("");
          }
        }
      }
    },
    [searchQuery, layoutNodes, setSelectedNode],
  );

  return (
    <div className="h-full w-full relative" ref={reactFlowWrapper} onKeyDown={onKeyDown} tabIndex={0}>
      {/* Search overlay */}
      {searchOpen && (
        <div className="absolute top-2 left-1/2 -translate-x-1/2 z-50">
          <input
            ref={searchInputRef}
            className="bg-gray-800 border border-gray-500 rounded-lg px-3 py-1.5 text-sm text-white w-64 focus:border-blue-500 focus:outline-none shadow-lg"
            value={searchQuery}
            onChange={(e) => setSearchQuery(e.target.value)}
            onKeyDown={handleSearchKeyDown}
            placeholder="Search nodes... (Enter to select, Esc to close)"
            autoFocus
          />
        </div>
      )}
      <ReactFlow
        nodes={displayNodes}
        edges={graphEdges}
        nodeTypes={nodeTypes}
        edgeTypes={edgeTypes}
        onNodesChange={onNodesChange}
        onEdgesChange={onEdgesChange}
        onNodeClick={onNodeClick}
        onEdgeClick={onEdgeClick}
        onPaneClick={onPaneClick}
        onNodeContextMenu={onNodeContextMenu}
        onConnect={onConnect}
        isValidConnection={isValidConnection}
        onDragOver={onDragOver}
        onDrop={onDrop}
        fitView
        colorMode="dark"
      >
        <Background />
        <Controls />
        <MiniMap />
      </ReactFlow>
      {contextMenu && (
        <NodeContextMenu
          x={contextMenu.x}
          y={contextMenu.y}
          nodeId={contextMenu.nodeId}
          isTerminal={contextMenu.nodeId === "done" || contextMenu.nodeId === "fail"}
          isEntry={activeWorkflow?.entry === contextMenu.nodeId}
          onSetEntry={() => {
            if (activeWorkflow) updateWorkflow(activeWorkflow.name, { entry: contextMenu.nodeId });
          }}
          onDuplicate={() => {
            const newName = duplicateNode(contextMenu.nodeId);
            if (newName) setSelectedNode(newName);
          }}
          onDelete={() => {
            removeNode(contextMenu.nodeId);
            clearSelection();
          }}
          onClose={() => setContextMenu(null)}
        />
      )}
    </div>
  );
}
