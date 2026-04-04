import { useCallback, useEffect, useMemo, useRef, useState, type DragEvent, type MouseEvent as ReactMouseEvent } from "react";
import { ReactFlow, Background, Controls, MiniMap, useReactFlow } from "@xyflow/react";
import type { NodeMouseHandler, EdgeMouseHandler, Node } from "@xyflow/react";
import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import { useUIStore } from "@/store/ui";
import { NODE_COLORS, DEBOUNCE_FIT_VIEW_MS, DEBOUNCE_LAYOUT_SETTLE_MS } from "@/lib/constants";
import { useActiveWorkflow } from "@/hooks/useActiveWorkflow";
import { useCanvasSearch } from "@/hooks/useCanvasSearch";
import { useCanvasKeyboard } from "@/hooks/useCanvasKeyboard";
import { useCanvasConnections } from "@/hooks/useCanvasConnections";
import { useCanvasLayout } from "@/hooks/useCanvasLayout";
import { useAddNode } from "@/hooks/useAddNode";
import { useFullscreen } from "@/hooks/useFullscreen";
import { isAuxiliaryNodeId } from "@/lib/documentToGraph";
import { isDetailNodeId, DETAIL_PREFIX_EDGE } from "@/lib/nodeDetailGraph";
import type { NodeKind } from "@/api/types";
import WorkflowNode from "./WorkflowNode";
import ConditionalEdge from "./ConditionalEdge";
import AuxiliaryNode from "./AuxiliaryNode";
import ReferenceEdge from "./ReferenceEdge";
import DetailSubNode from "./DetailSubNode";
import NodeContextMenu from "./NodeContextMenu";
import EditNodeModal from "@/components/Modals/EditNodeModal";
import EditEdgeModal from "@/components/Modals/EditEdgeModal";
import BreadcrumbBar from "./BreadcrumbBar";
import CanvasToolbar from "./CanvasToolbar";
import QuickAddMenu from "./QuickAddMenu";
import SearchOverlay from "./SearchOverlay";

const nodeTypes = { workflowNode: WorkflowNode, auxiliaryNode: AuxiliaryNode, detailSubNode: DetailSubNode };
const edgeTypes = { conditionalEdge: ConditionalEdge, referenceEdge: ReferenceEdge };

function isEditableNode(id: string): boolean {
  return id !== "__start__" && id !== "done" && id !== "fail" && !isAuxiliaryNodeId(id);
}

export default function Canvas() {
  const addNode = useAddNode();
  const removeNode = useDocumentStore((s) => s.removeNode);
  const duplicateNode = useDocumentStore((s) => s.duplicateNode);
  const updateWorkflow = useDocumentStore((s) => s.updateWorkflow);
  const setSelectedNode = useSelectionStore((s) => s.setSelectedNode);
  const setSelectedEdge = useSelectionStore((s) => s.setSelectedEdge);
  const clearSelection = useSelectionStore((s) => s.clearSelection);
  const selectedNodeId = useSelectionStore((s) => s.selectedNodeId);
  const setDetailNodeId = useUIStore((s) => s.setDetailNodeId);
  const subNodeViewStack = useUIStore((s) => s.subNodeViewStack);
  const pushSubNodeView = useUIStore((s) => s.pushSubNodeView);
  const activeWorkflow = useActiveWorkflow();
  const reactFlowWrapper = useRef<HTMLDivElement>(null);
  const { screenToFlowPosition, fitView } = useReactFlow();

  // Context menu state
  const [contextMenu, setContextMenu] = useState<{ x: number; y: number; nodeId: string } | null>(null);

  // Delegated hooks
  const layout = useCanvasLayout();
  const search = useCanvasSearch(layout.layoutNodes);
  const connections = useCanvasConnections();
  const { toggleFullscreen } = useFullscreen();
  const onKeyDown = useCanvasKeyboard({
    search,
    quickAddMenu: connections.quickAddMenu,
    setQuickAddMenu: (v) => connections.setQuickAddMenu(v),
    setContextMenu,
  });


  // Fit view when switching workflows
  const activeWorkflowName = activeWorkflow?.name;
  const prevWorkflowRef = useRef<string | undefined>(activeWorkflowName);
  useEffect(() => {
    if (prevWorkflowRef.current !== activeWorkflowName && activeWorkflowName) {
      prevWorkflowRef.current = activeWorkflowName;
      setTimeout(() => fitView({ padding: 0.2 }), DEBOUNCE_LAYOUT_SETTLE_MS);
    }
  }, [activeWorkflowName, fitView]);

  // Apply search filter: dim non-matching nodes, highlight current match
  const displayNodes = useMemo(
    () => search.applySearchFilter(layout.layoutNodes),
    [layout.layoutNodes, search.applySearchFilter],
  );

  const handleSearchKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      if (e.key === "Escape") {
        search.closeSearch();
      } else if (e.key === "Enter") {
        search.selectCurrentMatch();
        const id = search.matchedNodeIds[search.currentMatchIndex];
        if (id) fitView({ nodes: [{ id }], padding: 0.5 });
      } else if (e.key === "ArrowDown") {
        e.preventDefault();
        search.nextMatch();
      } else if (e.key === "ArrowUp") {
        e.preventDefault();
        search.prevMatch();
      }
    },
    [search, fitView],
  );

  // Node event handlers
  const onNodeClick: NodeMouseHandler = useCallback(
    (_event, node) => {
      connections.setQuickAddMenu(null);
      // In sub-node view, clicking sub-nodes is handled by DetailSubNode itself
      if (isAuxiliaryNodeId(node.id)) return;
      // Open editable modal on single click for editable nodes
      if (isEditableNode(node.id)) {
        setDetailNodeId(node.id);
      } else {
        setSelectedNode(node.id);
      }
    },
    [setSelectedNode, setDetailNodeId, connections],
  );

  const onNodeDoubleClick: NodeMouseHandler = useCallback(
    (_event, node) => {
      // In sub-node view: double-click on edge sub-node navigates to the target node
      if (node.id.startsWith(DETAIL_PREFIX_EDGE)) {
        const data = node.data as { targetNodeId?: string };
        if (data.targetNodeId && isEditableNode(data.targetNodeId)) {
          setDetailNodeId(null);
          pushSubNodeView(data.targetNodeId);
        }
        return;
      }
      if (isEditableNode(node.id) && !isDetailNodeId(node.id)) {
        // Close any open modal first
        setDetailNodeId(null);
        // Navigate into sub-node detail view
        pushSubNodeView(node.id);
      }
    },
    [setDetailNodeId, pushSubNodeView],
  );

  const onEdgeClick: EdgeMouseHandler = useCallback(
    (_event, edge) => {
      setSelectedEdge(edge.id);
      connections.setQuickAddMenu(null);
    },
    [setSelectedEdge, connections],
  );

  const onPaneClick = useCallback(() => {
    clearSelection();
    setContextMenu(null);
    connections.setQuickAddMenu(null);
    setDetailNodeId(null);
  }, [clearSelection, setDetailNodeId, connections]);

  const onNodeContextMenu = useCallback(
    (event: ReactMouseEvent, node: Node) => {
      event.preventDefault();
      if (isAuxiliaryNodeId(node.id)) return;
      setContextMenu({ x: event.clientX, y: event.clientY, nodeId: node.id });
    },
    [],
  );

  // Quick-add menu handler
  const handleQuickAdd = useCallback(
    (kind: NodeKind) => {
      if (!connections.quickAddMenu || !activeWorkflow) return;
      const position = screenToFlowPosition({ x: connections.quickAddMenu.x, y: connections.quickAddMenu.y });
      const name = addNode(kind);
      if (!name) return;
      layout.pendingPositionsRef.current.set(name, position);
      connections.addEdge(activeWorkflow.name, { from: connections.quickAddMenu.sourceId, to: name });
      connections.setQuickAddMenu(null);
    },
    [connections, activeWorkflow, addNode, screenToFlowPosition, layout.pendingPositionsRef],
  );

  // Drag-and-drop from palette
  const onDragOver = useCallback((e: DragEvent) => {
    e.preventDefault();
    e.dataTransfer.dropEffect = "move";
  }, []);

  const onDrop = useCallback(
    (e: DragEvent) => {
      e.preventDefault();
      const kind = e.dataTransfer.getData("application/iterion-node") as NodeKind;
      if (!kind || kind === "done" || kind === "fail") return;
      const position = screenToFlowPosition({ x: e.clientX, y: e.clientY });
      const name = addNode(kind);
      if (name) layout.pendingPositionsRef.current.set(name, position);
    },
    [addNode, screenToFlowPosition, layout.pendingPositionsRef],
  );

  // Toolbar actions
  const handleArrange = useCallback(() => layout.handleArrange(fitView), [layout, fitView]);
  const handleFitView = useCallback(() => fitView({ padding: 0.2 }), [fitView]);
  const handleFocusNode = useCallback(() => {
    if (selectedNodeId) fitView({ nodes: [{ id: selectedNodeId }], padding: 0.5 });
  }, [selectedNodeId, fitView]);

  return (
    <div className="h-full w-full relative" ref={reactFlowWrapper} onKeyDown={onKeyDown} tabIndex={0}>
      <CanvasToolbar
        onArrange={handleArrange}
        onFitView={handleFitView}
        onFocusNode={selectedNodeId ? handleFocusNode : null}
        onBrowserFullscreen={toggleFullscreen}
        onFitViewAfterDelay={() => setTimeout(() => fitView({ padding: 0.2 }), DEBOUNCE_FIT_VIEW_MS)}
      />

      {search.searchOpen && (
        <SearchOverlay
          ref={search.searchInputRef}
          searchQuery={search.searchQuery}
          onSearchChange={search.setSearchQuery}
          onKeyDown={handleSearchKeyDown}
          matchCount={search.matchedNodeIds.length}
          currentIndex={search.currentMatchIndex}
        />
      )}

      {/* Connection error feedback */}
      {connections.connectionError && (
        <div className="absolute bottom-4 left-1/2 -translate-x-1/2 z-50 bg-red-900/90 text-red-200 text-xs px-3 py-1.5 rounded-lg shadow-lg border border-red-700">
          {connections.connectionError}
        </div>
      )}

      <ReactFlow
        nodes={displayNodes}
        edges={layout.layoutEdges}
        nodeTypes={nodeTypes}
        edgeTypes={edgeTypes}
        onNodesChange={layout.onNodesChange}
        onEdgesChange={layout.onEdgesChange}
        onNodeClick={onNodeClick}
        onNodeDoubleClick={onNodeDoubleClick}
        onEdgeClick={onEdgeClick}
        onPaneClick={onPaneClick}
        onNodeContextMenu={onNodeContextMenu}
        onConnect={connections.onConnect}
        onConnectStart={connections.onConnectStart}
        onConnectEnd={connections.onConnectEnd}
        isValidConnection={connections.isValidConnection}
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
            const kind = (node.data as { kind?: string })?.kind as NodeKind | undefined;
            return kind ? (NODE_COLORS[kind] ?? "#6B7280") : "#6B7280";
          }}
        />
      </ReactFlow>

      {/* Context menu */}
      {contextMenu && (
        <NodeContextMenu
          x={contextMenu.x}
          y={contextMenu.y}
          nodeId={contextMenu.nodeId}
          isTerminal={contextMenu.nodeId === "done" || contextMenu.nodeId === "fail" || contextMenu.nodeId === "__start__"}
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

      {/* Breadcrumb for sub-node view */}
      {subNodeViewStack.length > 0 && <BreadcrumbBar />}

      {/* Editable node modal (single click) */}
      <EditNodeModal />

      {/* Edge editing modal */}
      <EditEdgeModal />

      {connections.quickAddMenu && (
        <QuickAddMenu
          x={connections.quickAddMenu.x}
          y={connections.quickAddMenu.y}
          sourceId={connections.quickAddMenu.sourceId}
          onAddNode={handleQuickAdd}
          onConnectTerminal={(target) => {
            if (activeWorkflow) {
              connections.addEdge(activeWorkflow.name, { from: connections.quickAddMenu!.sourceId, to: target });
              connections.setQuickAddMenu(null);
            }
          }}
          onClose={() => connections.setQuickAddMenu(null)}
        />
      )}
    </div>
  );
}
