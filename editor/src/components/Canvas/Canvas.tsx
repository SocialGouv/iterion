import { useCallback, useEffect, useMemo, useRef, useState, type DragEvent, type MouseEvent as ReactMouseEvent } from "react";
import { ReactFlow, Background, Controls, MiniMap, useReactFlow } from "@xyflow/react";
import type { NodeMouseHandler, EdgeMouseHandler, Node, Viewport } from "@xyflow/react";
import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import { useUIStore } from "@/store/ui";
import { useThemeStore } from "@/store/theme";
import { NODE_COLORS, DEBOUNCE_FIT_VIEW_MS, DEBOUNCE_LAYOUT_SETTLE_MS } from "@/lib/constants";
import { parseGroups } from "@/lib/groups";
import { useActiveWorkflow } from "@/hooks/useActiveWorkflow";
import { useCanvasSearch } from "@/hooks/useCanvasSearch";
import { useCanvasKeyboard } from "@/hooks/useCanvasKeyboard";
import { useCanvasConnections } from "@/hooks/useCanvasConnections";
import { useCanvasLayout } from "@/hooks/useCanvasLayout";
import { useAddNode } from "@/hooks/useAddNode";
import { useAddFromLibrary } from "@/hooks/useAddFromLibrary";
import { useAddSubNode, type SubNodeDragData } from "@/hooks/useAddSubNode";
import { useFullscreen } from "@/hooks/useFullscreen";
import { useLibraryStore, selectAllItems } from "@/store/library";
import { isAuxiliaryNodeId } from "@/lib/documentToGraph";
import { isGroupNodeId } from "@/lib/groups";
import { isDetailNodeId, DETAIL_PREFIX_EDGE, parseDetailId } from "@/lib/nodeDetailGraph";
import type { NodeKind } from "@/api/types";
import WorkflowNode from "./WorkflowNode";
import ConditionalEdge from "./ConditionalEdge";
import AuxiliaryNode from "./AuxiliaryNode";
import ReferenceEdge from "./ReferenceEdge";
import DetailSubNode from "./DetailSubNode";
import GroupNode from "./GroupNode";
import NodeContextMenu from "./NodeContextMenu";

import BreadcrumbBar from "./BreadcrumbBar";
import CanvasEmpty from "./CanvasEmpty";
import CanvasToolbar from "./CanvasToolbar";
import ToolPalette from "./ToolPalette";
import QuickAddMenu from "./QuickAddMenu";
import SchemaRoleDialog from "./SchemaRoleDialog";
import SearchOverlay from "./SearchOverlay";

const nodeTypes = { workflowNode: WorkflowNode, auxiliaryNode: AuxiliaryNode, detailSubNode: DetailSubNode, groupNode: GroupNode };
const edgeTypes = { conditionalEdge: ConditionalEdge, referenceEdge: ReferenceEdge };

function isEditableNode(id: string): boolean {
  return id !== "__start__" && id !== "done" && id !== "fail" && !isAuxiliaryNodeId(id) && !isGroupNodeId(id);
}

export default function Canvas() {
  const addNode = useAddNode();
  const addFromLibrary = useAddFromLibrary();
  const addSubNode = useAddSubNode();
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
  const resolvedTheme = useThemeStore((s) => s.resolved);
  const subNodeViewStack = useUIStore((s) => s.subNodeViewStack);
  const pushSubNodeView = useUIStore((s) => s.pushSubNodeView);
  const pendingFitNodeId = useUIStore((s) => s.pendingFitNodeId);
  const setPendingFitNodeId = useUIStore((s) => s.setPendingFitNodeId);
  const activeWorkflow = useActiveWorkflow();
  const reactFlowWrapper = useRef<HTMLDivElement>(null);
  const { screenToFlowPosition, fitView, getNodes, getViewport, setViewport } = useReactFlow();

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

  // Schema role dialog state (for existing schema drops without relation)
  const [schemaRoleDialog, setSchemaRoleDialog] = useState<{
    x: number; y: number; data: SubNodeDragData; centralNodeId: string; position: { x: number; y: number };
  } | null>(null);

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

  // URL-driven node centering ("Open in editor" from a run). The
  // EditorView puts the target ir_node_id into the UI store; we wait
  // for it to appear in React Flow's node set (the layout pass needs
  // a tick) before calling fitView, then clear the request so a later
  // navigation doesn't re-trigger.
  useEffect(() => {
    if (!pendingFitNodeId) return;
    const t = setTimeout(() => {
      const exists = getNodes().some((n) => n.id === pendingFitNodeId);
      if (exists) {
        fitView({ nodes: [{ id: pendingFitNodeId }], padding: 0.5, duration: 400 });
      }
      setPendingFitNodeId(null);
    }, DEBOUNCE_LAYOUT_SETTLE_MS);
    return () => clearTimeout(t);
  }, [pendingFitNodeId, fitView, getNodes, setPendingFitNodeId]);

  // Save/restore viewport when entering/leaving sub-node detail view
  const prevSubViewRef = useRef<string | null>(null);
  const savedViewportRef = useRef<Viewport | null>(null);
  useEffect(() => {
    const currentSubView = subNodeViewStack.length > 0
      ? subNodeViewStack[subNodeViewStack.length - 1]!
      : null;
    if (prevSubViewRef.current === currentSubView) return;
    const wasInSubView = prevSubViewRef.current !== null;
    prevSubViewRef.current = currentSubView;
    if (currentSubView !== null && !wasInSubView) {
      // Entering sub-node view from global: save viewport, then fitView
      savedViewportRef.current = getViewport();
      setTimeout(() => fitView({ padding: 0.2 }), DEBOUNCE_LAYOUT_SETTLE_MS);
    } else if (currentSubView === null && wasInSubView) {
      // Returning to global view: restore saved viewport
      const saved = savedViewportRef.current;
      if (saved) {
        setTimeout(() => setViewport(saved), DEBOUNCE_LAYOUT_SETTLE_MS);
        savedViewportRef.current = null;
      } else {
        setTimeout(() => fitView({ padding: 0.2 }), DEBOUNCE_LAYOUT_SETTLE_MS);
      }
    } else {
      // Navigating between sub-node views: fitView
      setTimeout(() => fitView({ padding: 0.2 }), DEBOUNCE_LAYOUT_SETTLE_MS);
    }
  }, [subNodeViewStack, fitView, getViewport, setViewport]);

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
      if (isAuxiliaryNodeId(node.id)) return;

      const detail = parseDetailId(node.id);
      if (detail) {
        if (detail.kind === "central") {
          const label = (node.data as { label?: string }).label;
          if (label) setSelectedNode(label);
        } else if (detail.kind === "tool") {
          setSelectedNode(detail.name);
        }
        // schema/prompt/var/edge: DetailSubNode's onClick drives the action.
        return;
      }

      if (node.id.startsWith(DETAIL_PREFIX_EDGE)) {
        return;
      }

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

      // Subnode drop (in detail view)
      const subNodeJson = e.dataTransfer.getData("application/iterion-subnode");
      if (subNodeJson && subNodeViewStack.length > 0) {
        try {
          const data = JSON.parse(subNodeJson) as SubNodeDragData;
          const centralNodeId = subNodeViewStack[subNodeViewStack.length - 1]!;

          // Existing items without relation need a role picker for schemas
          if (data.subKind === "schema" && !data.relation && data.existingName) {
            setSchemaRoleDialog({ x: e.clientX, y: e.clientY, data, centralNodeId, position });
            return;
          }

          const predictedId = addSubNode(data, centralNodeId);
          if (predictedId) layout.pendingPositionsRef.current.set(predictedId, position);
        } catch { /* invalid JSON */ }
        return;
      }

      // Block workflow node drops in subnode view
      if (subNodeViewStack.length > 0) return;

      // Library item drop (single node or multi-node pattern)
      const libraryItemId = e.dataTransfer.getData("application/iterion-library");
      if (libraryItemId) {
        const item = allLibraryItems.find((i) => i.id === libraryItemId);
        if (item) {
          const result = addFromLibrary(item);
          if (result) {
            if (Array.isArray(result)) {
              result.forEach((name, i) => {
                layout.pendingPositionsRef.current.set(name, {
                  x: position.x,
                  y: position.y + (i - (result.length - 1) / 2) * 150,
                });
              });
            } else {
              layout.pendingPositionsRef.current.set(result, position);
            }
          }
        }
        return;
      }

      // Generic node drop
      const kind = e.dataTransfer.getData("application/iterion-node") as NodeKind;
      if (!kind || kind === "done" || kind === "fail") return;
      const name = addNode(kind);
      if (name) layout.pendingPositionsRef.current.set(name, position);
    },
    [addNode, addFromLibrary, addSubNode, allLibraryItems, screenToFlowPosition, layout.pendingPositionsRef, subNodeViewStack],
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
        <div className="absolute bottom-4 left-1/2 -translate-x-1/2 z-50 bg-danger-soft text-danger-fg text-xs px-3 py-1.5 rounded-lg shadow-lg border border-danger">
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
        minZoom={0.05}
        maxZoom={4}
        selectionOnDrag={canvasTool === "select"}
        panOnDrag={canvasTool === "select" ? [1, 2] : true}
        multiSelectionKeyCode="Shift"
        colorMode={resolvedTheme}
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

      {/* Empty-state overlay when the document has no editable nodes */}
      {document &&
        document.agents.length === 0 &&
        document.judges.length === 0 &&
        document.routers.length === 0 &&
        document.humans.length === 0 &&
        document.tools.length === 0 && <CanvasEmpty />}

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

      {schemaRoleDialog && (
        <SchemaRoleDialog
          x={schemaRoleDialog.x}
          y={schemaRoleDialog.y}
          onSelect={(role) => {
            const { data, centralNodeId, position } = schemaRoleDialog;
            const predictedId = addSubNode({ ...data, relation: role }, centralNodeId);
            if (predictedId) layout.pendingPositionsRef.current.set(predictedId, position);
            setSchemaRoleDialog(null);
          }}
          onClose={() => setSchemaRoleDialog(null)}
        />
      )}

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
