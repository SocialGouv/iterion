import { useCallback, useEffect, useMemo, useRef, useState, type DragEvent, type KeyboardEvent, type MouseEvent as ReactMouseEvent } from "react";
import { ReactFlow, Background, Controls, MiniMap, useReactFlow } from "@xyflow/react";
import type { NodeMouseHandler, EdgeMouseHandler, Connection, Node, NodeChange, EdgeChange, Edge as FlowEdge } from "@xyflow/react";
import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import { useUIStore } from "@/store/ui";
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

const QUICK_ADD_TYPES: { kind: NodeKind; icon: string; label: string }[] = [
  { kind: "agent", icon: "\u{1F916}", label: "Agent" },
  { kind: "judge", icon: "\u{2696}\u{FE0F}", label: "Judge" },
  { kind: "router", icon: "\u{1F504}", label: "Router" },
  { kind: "join", icon: "\u{1F91D}", label: "Join" },
  { kind: "human", icon: "\u{1F464}", label: "Human" },
  { kind: "tool", icon: "\u{1F527}", label: "Tool" },
];

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
  const copiedNodeId = useSelectionStore((s) => s.copiedNodeId);
  const setCopiedNode = useSelectionStore((s) => s.setCopiedNode);
  const addToast = useUIStore((s) => s.addToast);
  const expanded = useUIStore((s) => s.expanded);
  const toggleExpanded = useUIStore((s) => s.toggleExpanded);
  const browserFullscreen = useUIStore((s) => s.browserFullscreen);
  const setBrowserFullscreen = useUIStore((s) => s.setBrowserFullscreen);
  const activeWorkflow = useActiveWorkflow();
  const reactFlowWrapper = useRef<HTMLDivElement>(null);
  const { screenToFlowPosition, fitView } = useReactFlow();

  // Context menu state
  const [contextMenu, setContextMenu] = useState<{ x: number; y: number; nodeId: string } | null>(null);

  // Search state
  const [searchQuery, setSearchQuery] = useState("");
  const [searchOpen, setSearchOpen] = useState(false);
  const searchInputRef = useRef<HTMLInputElement>(null);

  // Connection error feedback
  const [connectionError, setConnectionError] = useState<string | null>(null);
  const connectionErrorTimer = useRef<ReturnType<typeof setTimeout>>(undefined);

  // Quick-add menu state (shown when dragging from handle to empty canvas)
  const [quickAddMenu, setQuickAddMenu] = useState<{ x: number; y: number; sourceId: string } | null>(null);
  const pendingConnectSourceRef = useRef<string | null>(null);
  const edgeCountBeforeConnectRef = useRef<number>(0);

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
      setQuickAddMenu(null);
    },
    [setSelectedNode],
  );

  const onEdgeClick: EdgeMouseHandler = useCallback(
    (_event, edge) => {
      setSelectedEdge(edge.id);
      setQuickAddMenu(null);
    },
    [setSelectedEdge],
  );

  const onPaneClick = useCallback(() => {
    clearSelection();
    setContextMenu(null);
    setQuickAddMenu(null);
  }, [clearSelection]);

  const onNodeContextMenu = useCallback(
    (event: ReactMouseEvent, node: Node) => {
      event.preventDefault();
      setContextMenu({ x: event.clientX, y: event.clientY, nodeId: node.id });
    },
    [],
  );

  const getConnectionError = useCallback(
    (connection: FlowEdge | Connection): string | null => {
      if (!connection.source || !connection.target) return "Invalid connection";
      if (connection.source === connection.target) return "Cannot connect a node to itself";
      if (connection.source === "done" || connection.source === "fail") return "Cannot connect from a terminal node";
      if (activeWorkflow) {
        const dup = activeWorkflow.edges.some(
          (e) => e.from === connection.source && e.to === connection.target,
        );
        if (dup) return "This connection already exists";
      }
      return null;
    },
    [activeWorkflow],
  );

  const isValidConnection = useCallback(
    (connection: FlowEdge | Connection) => getConnectionError(connection) === null,
    [getConnectionError],
  );

  const onConnect = useCallback(
    (connection: Connection) => {
      if (!document || !connection.source || !connection.target) return;
      const workflowName = activeWorkflow?.name;
      if (!workflowName) return;
      const error = getConnectionError(connection);
      if (error) {
        setConnectionError(error);
        clearTimeout(connectionErrorTimer.current);
        connectionErrorTimer.current = setTimeout(() => setConnectionError(null), 2000);
        return;
      }
      addEdge(workflowName, { from: connection.source, to: connection.target });
    },
    [document, activeWorkflow, addEdge, getConnectionError],
  );

  const onConnectStart = useCallback(
    (_event: unknown, params: { nodeId: string | null }) => {
      setConnectionError(null);
      pendingConnectSourceRef.current = params.nodeId;
      edgeCountBeforeConnectRef.current = activeWorkflow?.edges?.length ?? 0;
    },
    [activeWorkflow],
  );

  const onConnectEnd = useCallback(
    (event: MouseEvent | TouchEvent) => {
      const source = pendingConnectSourceRef.current;
      pendingConnectSourceRef.current = null;

      if (!source || !document || !activeWorkflow) return;

      // Read fresh state to avoid stale closure after addEdge in onConnect
      const freshDoc = useDocumentStore.getState().document;
      const freshWorkflow = freshDoc?.workflows.find((w) => w.name === activeWorkflow.name);
      const currentEdgeCount = freshWorkflow?.edges?.length ?? 0;
      if (currentEdgeCount > edgeCountBeforeConnectRef.current) return;

      // No edge created — show quick-add menu at the drop position
      const clientX = "clientX" in event ? event.clientX : event.changedTouches?.[0]?.clientX ?? 0;
      const clientY = "clientY" in event ? event.clientY : event.changedTouches?.[0]?.clientY ?? 0;

      // Only show if drop was on the canvas (not on a node)
      const target = event.target as HTMLElement;
      if (target.closest(".react-flow__node")) return;

      setQuickAddMenu({ x: clientX, y: clientY, sourceId: source });
    },
    [document, activeWorkflow],
  );

  const handleQuickAdd = useCallback(
    (kind: NodeKind) => {
      if (!quickAddMenu || !document || !activeWorkflow) return;

      const existingNames = getAllNodeNames(document);
      const name = generateUniqueName(kind, existingNames);

      // Place at the drop position
      const position = screenToFlowPosition({ x: quickAddMenu.x, y: quickAddMenu.y });
      pendingPositionsRef.current.set(name, position);

      switch (kind) {
        case "agent": addAgent(defaultAgent(name)); break;
        case "judge": addJudge(defaultJudge(name)); break;
        case "router": addRouter(defaultRouter(name)); break;
        case "join": addJoin(defaultJoin(name)); break;
        case "human": addHuman(defaultHuman(name)); break;
        case "tool": addTool(defaultTool(name)); break;
      }

      // Create edge from source to new node
      addEdge(activeWorkflow.name, { from: quickAddMenu.sourceId, to: name });
      setSelectedNode(name);
      setQuickAddMenu(null);
    },
    [quickAddMenu, document, activeWorkflow, addAgent, addJudge, addRouter, addJoin, addHuman, addTool, addEdge, setSelectedNode, screenToFlowPosition],
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
        if (expanded) { toggleExpanded(); return; }
        if (searchOpen) { setSearchOpen(false); setSearchQuery(""); return; }
        if (quickAddMenu) { setQuickAddMenu(null); return; }
        clearSelection();
        setContextMenu(null);
        return;
      }

      // Copy/Paste
      if ((e.ctrlKey || e.metaKey) && e.key === "c" && !isInput) {
        if (selectedNodeId && selectedNodeId !== "done" && selectedNodeId !== "fail") {
          setCopiedNode(selectedNodeId);
          addToast("Node copied", "info");
        }
        return;
      }
      if ((e.ctrlKey || e.metaKey) && e.key === "v" && !isInput) {
        if (copiedNodeId) {
          const newName = duplicateNode(copiedNodeId);
          if (newName) {
            setSelectedNode(newName);
            addToast(`Pasted as ${newName}`, "success");
          }
        }
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
    [selectedNodeId, selectedEdgeId, document, removeNode, removeEdge, clearSelection, searchOpen, quickAddMenu, copiedNodeId, duplicateNode, setCopiedNode, setSelectedNode, addToast, expanded, toggleExpanded],
  );

  // Sync browser fullscreen state
  useEffect(() => {
    const handler = () => setBrowserFullscreen(!!window.document.fullscreenElement);
    window.document.addEventListener("fullscreenchange", handler);
    return () => window.document.removeEventListener("fullscreenchange", handler);
  }, [setBrowserFullscreen]);

  const handleBrowserFullscreen = useCallback(() => {
    if (window.document.fullscreenElement) {
      window.document.exitFullscreen();
    } else {
      window.document.documentElement.requestFullscreen();
    }
  }, []);

  // Fit view when switching workflows
  const prevWorkflowRef = useRef<string | undefined>(activeWorkflowName);
  useEffect(() => {
    if (prevWorkflowRef.current !== activeWorkflowName && activeWorkflowName) {
      prevWorkflowRef.current = activeWorkflowName;
      // Delay to let layout settle
      setTimeout(() => fitView({ padding: 0.2 }), 300);
    }
  }, [activeWorkflowName, fitView]);

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

  const handleArrange = useCallback(() => {
    autoLayout(graphNodes, graphEdges)
      .then((laid) => {
        setLayoutNodes(laid);
        prevTopologyRef.current = "";
        setTimeout(() => fitView({ padding: 0.2 }), 50);
      })
      .catch(() => {});
  }, [graphNodes, graphEdges, fitView]);

  const handleFitView = useCallback(() => {
    fitView({ padding: 0.2 });
  }, [fitView]);

  const handleFocusNode = useCallback(() => {
    if (selectedNodeId) {
      fitView({ nodes: [{ id: selectedNodeId }], padding: 0.5 });
    }
  }, [selectedNodeId, fitView]);

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

      {/* Fit view / Focus / Fullscreen buttons */}
      <div className="absolute top-2 right-2 z-40 flex gap-1">
        <button
          className="bg-gray-800/90 hover:bg-gray-700 border border-gray-600 text-xs px-2 py-1 rounded text-gray-300"
          onClick={handleArrange}
          title="Auto-arrange nodes chronologically"
        >
          Arrange
        </button>
        <button
          className="bg-gray-800/90 hover:bg-gray-700 border border-gray-600 text-xs px-2 py-1 rounded text-gray-300"
          onClick={handleFitView}
          title="Fit all nodes in view"
        >
          Fit
        </button>
        {selectedNodeId && (
          <button
            className="bg-gray-800/90 hover:bg-gray-700 border border-gray-600 text-xs px-2 py-1 rounded text-gray-300"
            onClick={handleFocusNode}
            title="Zoom to selected node"
          >
            Focus
          </button>
        )}
        <button
          className={`border text-xs px-2 py-1 rounded ${
            expanded
              ? "bg-blue-600 hover:bg-blue-700 border-blue-500 text-white"
              : "bg-gray-800/90 hover:bg-gray-700 border-gray-600 text-gray-300"
          }`}
          onClick={() => { toggleExpanded(); setTimeout(() => fitView({ padding: 0.2 }), 100); }}
          title={expanded ? "Collapse canvas (Esc)" : "Expand canvas (hide chrome)"}
        >
          {expanded ? "Collapse" : "Expand"}
        </button>
        <button
          className={`border text-xs px-2 py-1 rounded ${
            browserFullscreen
              ? "bg-blue-600 hover:bg-blue-700 border-blue-500 text-white"
              : "bg-gray-800/90 hover:bg-gray-700 border-gray-600 text-gray-300"
          }`}
          onClick={() => { handleBrowserFullscreen(); setTimeout(() => fitView({ padding: 0.2 }), 100); }}
          title={browserFullscreen ? "Exit fullscreen" : "Enter fullscreen"}
        >
          {browserFullscreen ? "Exit FS" : "Fullscreen"}
        </button>
      </div>

      {/* Connection error feedback */}
      {connectionError && (
        <div className="absolute bottom-4 left-1/2 -translate-x-1/2 z-50 bg-red-900/90 text-red-200 text-xs px-3 py-1.5 rounded-lg shadow-lg border border-red-700">
          {connectionError}
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
        onConnectStart={onConnectStart}
        onConnectEnd={onConnectEnd}
        isValidConnection={isValidConnection}
        onDragOver={onDragOver}
        onDrop={onDrop}
        fitView
        colorMode="dark"
      >
        <Background />
        <Controls />
        <MiniMap
          style={{ width: 200, height: 150 }}
          zoomable
          pannable
          nodeColor={(node) => {
            const kind = (node.data as { kind?: string })?.kind;
            switch (kind) {
              case "agent": return "#4A90D9";
              case "judge": return "#7B68EE";
              case "router": return "#E67E22";
              case "join": return "#2ECC71";
              case "human": return "#E74C3C";
              case "tool": return "#8B6914";
              case "done": return "#22C55E";
              case "fail": return "#EF4444";
              default: return "#6B7280";
            }
          }}
        />
      </ReactFlow>

      {/* Context menu */}
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

      {/* Quick-add node menu (shown when dragging from handle to empty canvas) */}
      {quickAddMenu && (
        <div
          className="fixed bg-gray-800 border border-gray-600 rounded-lg shadow-xl z-50 py-1 min-w-[140px]"
          style={{
            left: Math.min(quickAddMenu.x, window.innerWidth - 160),
            top: Math.min(quickAddMenu.y, window.innerHeight - 340),
          }}
        >
          <div className="px-3 py-1 text-[10px] text-gray-500 uppercase tracking-wider">Add node</div>
          {QUICK_ADD_TYPES.map(({ kind, icon, label }) => (
            <button
              key={kind}
              className="w-full text-left px-3 py-1.5 hover:bg-gray-700 text-xs text-white flex items-center gap-2"
              onClick={() => handleQuickAdd(kind)}
            >
              <span>{icon}</span>
              {label}
            </button>
          ))}
          <div className="border-t border-gray-700 my-1" />
          <div className="px-3 py-1 text-[10px] text-gray-500 uppercase tracking-wider">Connect to</div>
          <button
            className="w-full text-left px-3 py-1.5 hover:bg-gray-700 text-xs text-white flex items-center gap-2"
            onClick={() => {
              if (activeWorkflow) {
                addEdge(activeWorkflow.name, { from: quickAddMenu.sourceId, to: "done" });
                setQuickAddMenu(null);
              }
            }}
          >
            <span>{"\u{2705}"}</span>
            done
          </button>
          <button
            className="w-full text-left px-3 py-1.5 hover:bg-gray-700 text-xs text-white flex items-center gap-2"
            onClick={() => {
              if (activeWorkflow) {
                addEdge(activeWorkflow.name, { from: quickAddMenu.sourceId, to: "fail" });
                setQuickAddMenu(null);
              }
            }}
          >
            <span>{"\u{274C}"}</span>
            fail
          </button>
          <div className="border-t border-gray-700 my-1" />
          <button
            className="w-full text-left px-3 py-1.5 hover:bg-gray-700 text-xs text-gray-400 flex items-center gap-2"
            onClick={() => setQuickAddMenu(null)}
          >
            Cancel
          </button>
        </div>
      )}
    </div>
  );
}
