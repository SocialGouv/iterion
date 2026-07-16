// useCanvasHandlers extracts the ~14 inline useCallback handlers that
// Canvas.tsx used to declare directly. The Canvas component is now
// the orchestrator (subscribes to the stores, owns the dialog/menu
// state, renders ReactFlow); this hook just packages the handler
// closures so the JSX side reads as a one-liner per ReactFlow prop.
//
// Behavior preserving: every closure body is the same as it was
// inline, with the same dep list. Watch React hooks exhaustive-deps —
// hook-returned setters (e.g. connections.setQuickAddMenu,
// layout.pendingPositionsRef) must be listed in deps the same way they
// were before, so the eslint rule keeps catching real drift.
import { useCallback } from "react";
import type {
  Dispatch,
  DragEvent,
  KeyboardEvent as ReactKeyboardEvent,
  MouseEvent as ReactMouseEvent,
  SetStateAction,
} from "react";
import type {
  Edge,
  EdgeMouseHandler,
  FitView,
  Node,
  NodeMouseHandler,
  XYPosition,
} from "@xyflow/react";

import type { NodeKind } from "@/api/types";
import type { SubNodeDragData } from "@/hooks/useAddSubNode";
import type { LibraryItem } from "@/lib/library/types";
import {
  DETAIL_PREFIX_EDGE,
  isDetailNodeId,
  parseDetailId,
} from "@/lib/nodeDetailGraph";
import { isAuxiliaryNodeId } from "@/lib/documentToGraph";
import { isGroupNodeId } from "@/lib/groups";
import { isSubbotChildId } from "@/lib/subbotGraph";

import type { useCanvasConnections } from "@/hooks/useCanvasConnections";
import type { useCanvasLayout } from "@/hooks/useCanvasLayout";
import type { useCanvasSearch } from "@/hooks/useCanvasSearch";

// Local copy so the hook can stay self-contained; mirrors the predicate
// in Canvas.tsx (which is also still defined there for the palette-action
// builder and the QuickAddMenu callback).
function isEditableNode(id: string): boolean {
  return (
    id !== "__start__" &&
    id !== "done" &&
    id !== "fail" &&
    !isAuxiliaryNodeId(id) &&
    !isGroupNodeId(id) &&
    !isSubbotChildId(id)
  );
}

interface SchemaRoleDialogState {
  x: number;
  y: number;
  data: SubNodeDragData;
  centralNodeId: string;
  position: { x: number; y: number };
}

interface ContextMenuState {
  x: number;
  y: number;
  nodeId: string;
}

interface ActiveWorkflowLike {
  name: string;
}

export interface UseCanvasHandlersDeps {
  // Hooks
  connections: ReturnType<typeof useCanvasConnections>;
  layout: ReturnType<typeof useCanvasLayout>;
  search: ReturnType<typeof useCanvasSearch>;

  // ReactFlow imperatives
  fitView: FitView;
  screenToFlowPosition: (point: XYPosition) => XYPosition;

  // Selection store setters
  setSelectedNode: (id: string | null) => void;
  setSelectedEdge: (id: string | null) => void;
  clearSelection: () => void;
  selectedNodeId: string | null;

  // UI store
  pushSubNodeView: (id: string) => void;
  subNodeViewStack: string[];

  // Dialog state (owned by Canvas)
  setContextMenu: Dispatch<SetStateAction<ContextMenuState | null>>;
  setSchemaRoleDialog: Dispatch<SetStateAction<SchemaRoleDialogState | null>>;

  // Workflow + add helpers (match the upstream nullable returns exactly,
  // not the loose `undefined` shape — the body relies on truthy checks
  // that work for both, but the prop typing must align).
  activeWorkflow: ActiveWorkflowLike | undefined;
  addNode: (kind: NodeKind) => string | null;
  addFromLibrary: (item: LibraryItem) => string | string[] | null;
  addSubNode: (data: SubNodeDragData, centralNodeId: string) => string | null;
  allLibraryItems: LibraryItem[];
}

export interface CanvasHandlers {
  handleSearchKeyDown: (e: ReactKeyboardEvent) => void;
  selectFromNode: (node: Node) => void;
  onNodeClick: NodeMouseHandler;
  onNodeDoubleClick: NodeMouseHandler;
  onEdgeClick: EdgeMouseHandler;
  onPaneClick: () => void;
  onSelectionChange: (params: { nodes: Node[]; edges: Edge[] }) => void;
  onNodeContextMenu: (event: ReactMouseEvent, node: Node) => void;
  handleQuickAdd: (kind: NodeKind) => void;
  onDragOver: (e: DragEvent) => void;
  onDrop: (e: DragEvent) => void;
  handleArrange: () => void;
  handleFitView: () => void;
  handleFocusNode: () => void;
}

export function useCanvasHandlers(deps: UseCanvasHandlersDeps): CanvasHandlers {
  const {
    connections,
    layout,
    search,
    fitView,
    screenToFlowPosition,
    setSelectedNode,
    setSelectedEdge,
    clearSelection,
    selectedNodeId,
    pushSubNodeView,
    subNodeViewStack,
    setContextMenu,
    setSchemaRoleDialog,
    activeWorkflow,
    addNode,
    addFromLibrary,
    addSubNode,
    allLibraryItems,
  } = deps;

  const handleSearchKeyDown = useCallback(
    (e: ReactKeyboardEvent) => {
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

  // Maps a React Flow node to the studio selection store. Shared by the
  // mouse path (onNodeClick) and the keyboard path (onSelectionChange) so
  // the node-id dispatch (auxiliary / detail central|tool / edge / plain)
  // lives in one place.
  const selectFromNode = useCallback(
    (node: Node) => {
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
      if (node.id.startsWith(DETAIL_PREFIX_EDGE)) return;
      setSelectedNode(node.id);
    },
    [setSelectedNode],
  );

  const onNodeClick = useCallback<NodeMouseHandler>(
    (_event, node) => {
      connections.setQuickAddMenu(null);
      selectFromNode(node);
    },
    [selectFromNode, connections],
  );

  const onNodeDoubleClick = useCallback<NodeMouseHandler>(
    (_event, node) => {
      // In sub-node view: double-click on edge sub-node navigates to the target node
      if (node.id.startsWith(DETAIL_PREFIX_EDGE)) {
        const data = node.data as { targetNodeId?: string };
        if (data.targetNodeId && isEditableNode(data.targetNodeId)) {
          pushSubNodeView(data.targetNodeId);
        }
        return;
      }
      // Subbot nodes have no sub-node detail view (their "detail" is the
      // child file, opened via the frame's open button / Inspector).
      if ((node.data as { kind?: string })?.kind === "subbot") return;
      if (isEditableNode(node.id) && !isDetailNodeId(node.id)) {
        // Navigate into sub-node detail view
        pushSubNodeView(node.id);
      }
    },
    [pushSubNodeView],
  );

  const onEdgeClick = useCallback<EdgeMouseHandler>(
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
  }, [clearSelection, connections, setContextMenu]);

  // Keyboard reachability. React Flow makes nodes focusable (Tab) and
  // fires this on Enter/Space selection — unlike onNodeClick, which is
  // mouse-only — so it's the one hook that catches keyboard selection.
  // Mirror a single selected node/edge into the studio selection store
  // so Tab→Enter opens the inspector exactly like a click. We do NOT
  // clear on an empty selection: onPaneClick and the Escape stack own
  // deselection, and acting on empty here would fight transient
  // selection resets during re-layout.
  const onSelectionChange = useCallback(
    ({ nodes, edges }: { nodes: Node[]; edges: Edge[] }) => {
      const node = nodes.length === 1 ? nodes[0] : undefined;
      if (node) {
        selectFromNode(node);
        return;
      }
      const edge = nodes.length === 0 && edges.length === 1 ? edges[0] : undefined;
      if (edge) setSelectedEdge(edge.id);
    },
    [selectFromNode, setSelectedEdge],
  );

  const onNodeContextMenu = useCallback(
    (event: ReactMouseEvent, node: Node) => {
      event.preventDefault();
      // Subbot child nodes belong to another file — no edit context menu.
      if (isAuxiliaryNodeId(node.id) || isDetailNodeId(node.id) || isSubbotChildId(node.id)) return;
      setContextMenu({ x: event.clientX, y: event.clientY, nodeId: node.id });
    },
    [setContextMenu],
  );

  // Quick-add menu handler
  const handleQuickAdd = useCallback(
    (kind: NodeKind) => {
      if (!connections.quickAddMenu || !activeWorkflow) return;
      const position = screenToFlowPosition({
        x: connections.quickAddMenu.x,
        y: connections.quickAddMenu.y,
      });
      const name = addNode(kind);
      if (!name) return;
      layout.pendingPositionsRef.current.set(name, position);
      connections.addEdge(activeWorkflow.name, {
        from: connections.quickAddMenu.sourceId,
        to: name,
      });
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
        } catch {
          /* invalid JSON */
        }
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
    [
      addNode,
      addFromLibrary,
      addSubNode,
      allLibraryItems,
      screenToFlowPosition,
      layout.pendingPositionsRef,
      subNodeViewStack,
      setSchemaRoleDialog,
    ],
  );

  // Toolbar actions
  const handleArrange = useCallback(
    () => layout.handleArrange(fitView),
    [layout, fitView],
  );
  const handleFitView = useCallback(() => fitView({ padding: 0.2 }), [fitView]);
  const handleFocusNode = useCallback(() => {
    if (selectedNodeId) fitView({ nodes: [{ id: selectedNodeId }], padding: 0.5 });
  }, [selectedNodeId, fitView]);

  return {
    handleSearchKeyDown,
    selectFromNode,
    onNodeClick,
    onNodeDoubleClick,
    onEdgeClick,
    onPaneClick,
    onSelectionChange,
    onNodeContextMenu,
    handleQuickAdd,
    onDragOver,
    onDrop,
    handleArrange,
    handleFitView,
    handleFocusNode,
  };
}
