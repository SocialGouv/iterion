import { useCallback, useEffect, useMemo, useRef, useState, type DragEvent, type MouseEvent as ReactMouseEvent } from "react";
import { ReactFlow, Background, Controls, MiniMap, useReactFlow } from "@xyflow/react";
import type { NodeMouseHandler, EdgeMouseHandler, Node } from "@xyflow/react";
import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import { useUIStore } from "@/store/ui";
import { NODE_COLORS, DEBOUNCE_FIT_VIEW_MS, DEBOUNCE_LAYOUT_SETTLE_MS } from "@/lib/constants";
import { parseGroups } from "@/lib/groups";
import { useActiveWorkflow } from "@/hooks/useActiveWorkflow";
import { useCanvasSearch } from "@/hooks/useCanvasSearch";
import { useCanvasKeyboard } from "@/hooks/useCanvasKeyboard";
import { useCanvasConnections } from "@/hooks/useCanvasConnections";
import { useCanvasLayout } from "@/hooks/useCanvasLayout";
import { useAddNode } from "@/hooks/useAddNode";
import { useAddFromLibrary } from "@/hooks/useAddFromLibrary";
import { useFullscreen } from "@/hooks/useFullscreen";
import { useLibraryStore, selectAllItems } from "@/store/library";
import { isAuxiliaryNodeId } from "@/lib/documentToGraph";
import { isGroupNodeId } from "@/lib/groups";
import { isDetailNodeId, DETAIL_PREFIX_EDGE } from "@/lib/nodeDetailGraph";
import type { NodeKind } from "@/api/types";
import WorkflowNode from "./WorkflowNode";
import ConditionalEdge from "./ConditionalEdge";
import AuxiliaryNode from "./AuxiliaryNode";
import ReferenceEdge from "./ReferenceEdge";
import DetailSubNode from "./DetailSubNode";
import GroupNode from "./GroupNode";
import NodeContextMenu from "./NodeContextMenu";

import EditEdgeModal from "@/components/Modals/EditEdgeModal";
import BreadcrumbBar from "./BreadcrumbBar";
import CanvasToolbar from "./CanvasToolbar";
import ToolPalette from "./ToolPalette";
import QuickAddMenu from "./QuickAddMenu";
import SearchOverlay from "./SearchOverlay";

const nodeTypes = { workflowNode: WorkflowNode, auxiliaryNode: AuxiliaryNode, detailSubNode: DetailSubNode, groupNode: GroupNode };
const edgeTypes = { conditionalEdge: ConditionalEdge, referenceEdge: ReferenceEdge };

function isEditableNode(id: string): boolean {
  return id !== "__start__" && id !== "done" && id !== "fail" && !isAuxiliaryNodeId(id) && !isGroupNodeId(id);
}

export default function Canvas() {
  const addNode = useAddNode();
  const addFromLibrary = useAddFromLibrary();
  const allLibraryItems = useLibraryStore(selectAllItems);
  const document = useDocumentStore((s) => s.document);
  const removeNode = useDocumentStore((s) => s.removeNode);
  const duplicateNode = useDocumentStore((s) => s.duplicateNode);
  const updateWorkflow = useDocumentStore((s) => s.updateWorkflow);
  const addGroup = useDocumentStore((s) => s.addGroup);
  const removeGroup = useDocumentStore((s) => s.removeGroup);
  const updateGroup = useDocumentStore((s) => s.updateGroup);
  const setSelectedNode = useSelectionStore((s) => s.setSelectedNode);
  const setSelectedEdge = useSelectionStore((s) => s.setSelectedEdge);
  const clearSelection = useSelectionStore((s) => s.clearSelection);
  const selectedNodeId = useSelectionStore((s) => s.selectedNodeId);

  const canvasTool = useUIStore((s) => s.canvasTool);
  const subNodeViewStack = useUIStore((s) => s.subNodeViewStack);
  const pushSubNodeView = useUIStore((s) => s.pushSubNodeView);
  const activeWorkflow = useActiveWorkflow();
  const reactFlowWrapper = useRef<HTMLDivElement>(null);
  const { screenToFlowPosition, fitView, getNodes } = useReactFlow();

  // Parse groups for context menu
  const groups = useMemo(() => {
    if (!document) return [];
    return parseGroups(document.comments ?? []);
  }, [document]);

  // Build nodeId -> groupName lookup for context menu
  const nodeToGroup = useMemo(() => {
    const map = new Map<string, string>();
    for (const g of groups) {
      for (const nid of g.nodeIds) map.set(nid, g.name);
    }
    return map;
  }, [groups]);

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

  // Apply search filter: dim non-matching nodes, highlight current match.
  // Note: applySearchFilter is intentionally excluded from deps — its internal
  // state (searchOpen, searchQuery, matchedNodeIds) is captured in the callback.
  // Including it would create a dependency cycle since it also depends on layoutNodes.
  const { applySearchFilter } = search;
  const displayNodes = useMemo(
    () => applySearchFilter(layout.layoutNodes),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [layout.layoutNodes, search.searchOpen, search.searchQuery, search.currentMatchIndex],
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
      setSelectedNode(node.id);
    },
    [setSelectedNode, connections],
  );

  const onNodeDoubleClick: NodeMouseHandler = useCallback(
    (_event, node) => {
      // In sub-node view: double-click on edge sub-node navigates to the target node
      if (node.id.startsWith(DETAIL_PREFIX_EDGE)) {
        const data = node.data as { targetNodeId?: string };
        if (data.targetNodeId && isEditableNode(data.targetNodeId)) {
          pushSubNodeView(data.targetNodeId);
        }
        return;
      }
      if (isEditableNode(node.id) && !isDetailNodeId(node.id)) {
        // Navigate into sub-node detail view
        pushSubNodeView(node.id);
      }
    },
    [pushSubNodeView],
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
  }, [clearSelection, connections]);

  const onNodeContextMenu = useCallback(
    (event: ReactMouseEvent, node: Node) => {
      event.preventDefault();
      if (isAuxiliaryNodeId(node.id) || isDetailNodeId(node.id)) return;
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
      const position = screenToFlowPosition({ x: e.clientX, y: e.clientY });

      // Library item drop
      const libraryItemId = e.dataTransfer.getData("application/iterion-library");
      if (libraryItemId) {
        const item = allLibraryItems.find((i) => i.id === libraryItemId);
        if (item) {
          const name = addFromLibrary(item);
          if (name) layout.pendingPositionsRef.current.set(name, position);
        }
        return;
      }

      // Generic node drop
      const kind = e.dataTransfer.getData("application/iterion-node") as NodeKind;
      if (!kind || kind === "done" || kind === "fail") return;
      const name = addNode(kind);
      if (name) layout.pendingPositionsRef.current.set(name, position);
    },
    [addNode, addFromLibrary, allLibraryItems, screenToFlowPosition, layout.pendingPositionsRef],
  );

  // Toolbar actions
  const handleArrange = useCallback(() => layout.handleArrange(fitView), [layout, fitView]);
  const handleFitView = useCallback(() => fitView({ padding: 0.2 }), [fitView]);
  const handleFocusNode = useCallback(() => {
    if (selectedNodeId) fitView({ nodes: [{ id: selectedNodeId }], padding: 0.5 });
  }, [selectedNodeId, fitView]);

  return (
    <div className={`h-full w-full relative${canvasTool === "pan" ? " cursor-grab" : ""}`} ref={reactFlowWrapper} onKeyDown={onKeyDown} tabIndex={0}>
      <ToolPalette />
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
        selectionOnDrag={canvasTool === "select"}
        panOnDrag={canvasTool === "select" ? [1, 2] : true}
        multiSelectionKeyCode="Shift"
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
          selectedNodeIds={getNodes().filter((n) => n.selected).map((n) => n.id)}
          belongsToGroup={nodeToGroup.get(contextMenu.nodeId) ?? null}
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
          onCreateGroup={(name, nodeIds) => {
            addGroup({ name, nodeIds });
          }}
          onRemoveGroup={(groupName) => {
            removeGroup(groupName);
          }}
          onRemoveFromGroup={(groupName, nodeId) => {
            const group = groups.find((g) => g.name === groupName);
            if (group) {
              const remaining = group.nodeIds.filter((id) => id !== nodeId);
              if (remaining.length < 2) removeGroup(groupName);
              else updateGroup(groupName, { nodeIds: remaining });
            }
          }}
          onClose={() => setContextMenu(null)}
        />
      )}

      {/* Breadcrumb for sub-node view */}
      {subNodeViewStack.length > 0 && <BreadcrumbBar />}

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
