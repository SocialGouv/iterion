import { useEffect, useMemo, useRef, useState } from "react";
import { ReactFlow, Background, Controls, MiniMap, useReactFlow } from "@xyflow/react";
import type { Viewport } from "@xyflow/react";
import { useDocumentStore, useDocumentStoreInstance } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import { useUIStore } from "@/store/ui";
import { useThemeStore } from "@/store/theme";
import { NODE_COLORS, DEBOUNCE_FIT_VIEW_MS, DEBOUNCE_LAYOUT_SETTLE_MS, type LayerKind } from "@/lib/constants";
import { parseGroups } from "@/lib/groups";
import { FLOW_CONTROLS_STYLE, FLOW_MINIMAP_BG, FLOW_MINIMAP_MASK, FLOW_MINIMAP_STYLE } from "@/lib/flowTheme";
import { useActiveWorkflow } from "@/hooks/useActiveWorkflow";
import { useCanvasSearch } from "@/hooks/useCanvasSearch";
import { useCanvasKeyboard } from "@/hooks/useCanvasKeyboard";
import { useCanvasConnections } from "@/hooks/useCanvasConnections";
import { useCanvasLayout } from "@/hooks/useCanvasLayout";
import { useAddNode } from "@/hooks/useAddNode";
import { useAddFromLibrary } from "@/hooks/useAddFromLibrary";
import { useAddSubNode, type SubNodeDragData } from "@/hooks/useAddSubNode";
import { useFullscreen } from "@/hooks/useFullscreen";
import { useConfirm } from "@/hooks/useConfirm";
import { useLibraryStore, selectAllItems } from "@/store/library";
import { documentHasEditableNodes, isAuxiliaryNodeId } from "@/lib/documentToGraph";
import { isGroupNodeId } from "@/lib/groups";
import type { NodeKind } from "@/api/types";
import WorkflowNode from "./WorkflowNode";
import ConditionalEdge from "./ConditionalEdge";
import AuxiliaryNode from "./AuxiliaryNode";
import ReferenceEdge from "./ReferenceEdge";
import DetailSubNode from "./DetailSubNode";
import GroupNode from "./GroupNode";
import SubbotFrameNode from "./SubbotFrameNode";
import FanoutFrame from "@/components/Runs/FanoutFrame";
import { computeFanoutRegions, buildFanoutFrames } from "@/lib/fanoutRegions";
import { isSubbotChildId } from "@/lib/subbotGraph";
import NodeContextMenu from "./NodeContextMenu";

import BreadcrumbBar from "./BreadcrumbBar";
import CanvasEmpty from "./CanvasEmpty";
import CanvasToolbar from "./CanvasToolbar";
import ToolPalette from "./ToolPalette";
import QuickAddMenu from "./QuickAddMenu";
import SchemaRoleDialog from "./SchemaRoleDialog";
import SearchOverlay from "./SearchOverlay";
import { useCanvasHandlers } from "./useCanvasHandlers";
import CommandPalette, { type CommandAction } from "@/components/shared/CommandPalette";
import { useLocation } from "wouter";

const nodeTypes = { workflowNode: WorkflowNode, auxiliaryNode: AuxiliaryNode, detailSubNode: DetailSubNode, groupNode: GroupNode, subbotFrame: SubbotFrameNode, fanoutFrame: FanoutFrame };
const edgeTypes = { conditionalEdge: ConditionalEdge, referenceEdge: ReferenceEdge };

function isEditableNode(id: string): boolean {
  return id !== "__start__" && id !== "done" && id !== "fail" && !isAuxiliaryNodeId(id) && !isGroupNodeId(id) && !isSubbotChildId(id);
}

interface CanvasProps {
  // Whether the hosting editor tab is currently visible. Inactive tabs
  // stay mounted with display:none; React Flow can't measure a hidden
  // container, so when the tab is shown again we restore the saved
  // viewport (or refit) to avoid a blank canvas. Defaults to true so
  // standalone mounts behave unchanged.
  active?: boolean;
}

export default function Canvas({ active = true }: CanvasProps) {
  const addNode = useAddNode();
  const addFromLibrary = useAddFromLibrary();
  const addSubNode = useAddSubNode();
  const allLibraryItems = useLibraryStore(selectAllItems);
  const docStore = useDocumentStoreInstance();
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

  // Command-palette state. Triggered by Cmd+K / Ctrl+K. Lives on the
  // Canvas because every action wired below depends on Canvas-scoped
  // handlers — promoting it higher would mean re-wiring the action list
  // through context. The window-level listener captures Cmd+K from any
  // focused element except text inputs.
  const [paletteOpen, setPaletteOpen] = useState(false);
  const [, setLocation] = useLocation();
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && (e.key === "k" || e.key === "K")) {
        const target = e.target as HTMLElement | null;
        // Allow Cmd+K to open the palette even from inputs — that's
        // the established VS Code / Linear convention. The shortcut
        // is rare enough in inputs that the override is net-positive.
        e.preventDefault();
        setPaletteOpen((v) => !v);
        // Drop focus on the underlying element so the palette's input
        // wins focus reliably.
        if (target && typeof target.blur === "function") target.blur();
      }
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, []);

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
    onSelectAll: layout.selectNodes,
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

  // Restore the viewport when this tab regains visibility. EditorTabsView
  // keeps inactive tabs mounted with display:none; React Flow can't
  // measure a 0×0 container, so a tab returned to focus would otherwise
  // render blank. We snapshot the viewport just before the tab is hidden
  // and re-apply it once the container is visible again (fitView is the
  // fallback when nothing was saved). Mirrors the sub-node save/restore
  // pattern above.
  const wasActiveRef = useRef(active);
  const tabViewportRef = useRef<Viewport | null>(null);
  useEffect(() => {
    if (active === wasActiveRef.current) return;
    wasActiveRef.current = active;
    if (!active) {
      // Going hidden: remember the still-correct viewport.
      tabViewportRef.current = getViewport();
      return;
    }
    // Becoming visible: re-apply once layout settles (the container is
    // measurable again after the display:none → block flip).
    const t = setTimeout(() => {
      if (getNodes().length === 0) return; // avoid React Flow's empty-fit warning
      const saved = tabViewportRef.current;
      if (saved) setViewport(saved);
      else fitView({ padding: 0.2 });
    }, DEBOUNCE_LAYOUT_SETTLE_MS);
    return () => clearTimeout(t);
  }, [active, getViewport, setViewport, fitView, getNodes]);

  // Apply search filter: dim non-matching nodes, highlight current match.
  // Note: applySearchFilter is intentionally excluded from deps — its internal
  // state (searchOpen, searchQuery, matchedNodeIds) is captured in the callback.
  // Including it would create a dependency cycle since it also depends on layoutNodes.
  const { applySearchFilter } = search;
  // fan_out_each replicated regions, detected statically from the document
  // (router mode + edges + wait_all convergence). Drives the dashed region
  // frame so the "loop body" is visible at edit time, not just in a run.
  const fanoutRegions = useMemo(
    () => computeFanoutRegions(document, activeWorkflow),
    [document, activeWorkflow],
  );
  const displayNodes = useMemo(
    () => {
      const base = applySearchFilter(layout.layoutNodes);
      const frames = buildFanoutFrames(layout.layoutNodes, fanoutRegions);
      return frames.length > 0 ? [...frames, ...base] : base;
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [layout.layoutNodes, fanoutRegions, search.searchOpen, search.searchQuery, search.currentMatchIndex],
  );

  // All canvas handlers (search, node/edge click, drop, toolbar) live in
  // a co-located hook so this component stays an orchestrator. Behavior +
  // deps are preserved exactly — see useCanvasHandlers for the bodies.
  const {
    handleSearchKeyDown,
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
  } = useCanvasHandlers({
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
  });

  // Expose Arrange / Fit-view to the top-level Toolbar (which sits
  // outside the ReactFlowProvider subtree and can't call useReactFlow
  // directly). The setter is stable across renders, so this effect
  // re-runs only when the handlers themselves change.
  const setCanvasActions = useUIStore((s) => s.setCanvasActions);
  useEffect(() => {
    setCanvasActions({ arrange: handleArrange, fitView: handleFitView });
    return () => setCanvasActions(null);
  }, [setCanvasActions, handleArrange, handleFitView]);

  const { confirm, dialog } = useConfirm();

  return (
    <div className={`h-full w-full relative${canvasTool === "pan" ? " cursor-grab" : ""}`} ref={reactFlowWrapper} onKeyDown={onKeyDown} tabIndex={0}>
      <ToolPalette />
      <CanvasToolbar
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

      <CommandPalette
        open={paletteOpen}
        onClose={() => setPaletteOpen(false)}
        actions={buildPaletteActions({
          selectedNodeId,
          fitView: () => fitView({ padding: 0.2 }),
          navigate: setLocation,
          undo: () => docStore.getState().undo(),
          redo: () => docStore.getState().redo(),
          duplicate: (id) => docStore.getState().duplicateNode(id),
          remove: (id) => docStore.getState().removeNode(id),
          toggleExpanded: () => useUIStore.getState().toggleExpanded(),
          toggleLayer: (layer) => useUIStore.getState().toggleLayer(layer),
          toggleLibrary: () => useUIStore.getState().toggleLibraryPanel(),
          openSearch: search.openSearch,
          openFilePicker: () => useUIStore.getState().setFilePickerOpen(true),
          clearSelection,
        })}
      />

      {/* Connection error feedback */}
      {connections.connectionError && (
        <div className="absolute bottom-4 left-1/2 -translate-x-1/2 z-[var(--z-canvas)] bg-danger-soft text-danger-fg text-xs px-3 py-1.5 rounded-lg shadow-[var(--shadow-md)] border border-danger">
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
        onSelectionChange={onSelectionChange}
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
        nodesFocusable
        // Free up Space so it activates the keyboard-focused node (React
        // Flow's node wrapper handles Enter/Space → select) instead of
        // being captured as pan-on-hold. Pan stays available via drag and
        // the pan tool.
        panActivationKeyCode={null}
        colorMode={resolvedTheme}
      >
        <Background />
        <Controls style={FLOW_CONTROLS_STYLE} />
        <MiniMap
          style={{ width: 200, height: 150, ...FLOW_MINIMAP_STYLE }}
          bgColor={FLOW_MINIMAP_BG}
          maskColor={FLOW_MINIMAP_MASK}
          zoomable
          pannable
          nodeColor={(node) => {
            const kind = (node.data as { kind?: string })?.kind as NodeKind | undefined;
            return kind ? (NODE_COLORS[kind] ?? "var(--color-fg-subtle)") : "var(--color-fg-subtle)";
          }}
        />
      </ReactFlow>

      {/* Empty-state overlay when the document has no editable nodes.
          `computes` belongs here like every other kind: documentToGraph
          renders compute nodes, so omitting it put "No workflow loaded"
          OVER a workflow that has some — which is what a deterministic,
          LLM-free workflow (all compute) looks like. */}
      {document && !documentHasEditableNodes(document) && <CanvasEmpty />}

      {/* Context menu */}
      {contextMenu && (
        <NodeContextMenu
          x={contextMenu.x}
          y={contextMenu.y}
          nodeId={contextMenu.nodeId}
          isTerminal={contextMenu.nodeId === "done" || contextMenu.nodeId === "fail" || contextMenu.nodeId === "__start__"}
          isEntry={activeWorkflow?.entry === contextMenu.nodeId}
          selectedNodeIds={getNodes().filter((n) => n.selected && isEditableNode(n.id)).map((n) => n.id)}
          belongsToGroup={nodeToGroup.get(contextMenu.nodeId) ?? null}
          onSetEntry={() => {
            if (activeWorkflow) updateWorkflow(activeWorkflow.name, { entry: contextMenu.nodeId });
          }}
          onDuplicate={() => {
            const newName = duplicateNode(contextMenu.nodeId);
            if (newName) setSelectedNode(newName);
          }}
          onDelete={async () => {
            const id = contextMenu.nodeId;
            const ok = await confirm({
              title: "Delete node?",
              message: `Delete "${id}" and its edges? You can undo with Cmd/Ctrl+Z.`,
              confirmLabel: "Delete",
              confirmVariant: "danger",
            });
            if (!ok) return;
            removeNode(id);
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
      {dialog}
    </div>
  );
}

// buildPaletteActions assembles the Cmd+K action list. Kept outside the
// component so we don't recreate every action callback on every render
// (each action is just a thunk over store getters, which are stable).
function buildPaletteActions(deps: {
  selectedNodeId: string | null;
  fitView: () => void;
  navigate: (path: string) => void;
  undo: () => void;
  redo: () => void;
  duplicate: (id: string) => void;
  remove: (id: string) => void;
  toggleExpanded: () => void;
  toggleLayer: (l: LayerKind) => void;
  toggleLibrary: () => void;
  openSearch: () => void;
  openFilePicker: () => void;
  clearSelection: () => void;
}): CommandAction[] {
  const hasSelection = deps.selectedNodeId !== null && isEditableNode(deps.selectedNodeId);
  return [
    {
      id: "edit.undo",
      group: "Edit",
      title: "Undo",
      shortcut: "Ctrl+Z",
      keywords: ["revert", "back"],
      run: deps.undo,
    },
    {
      id: "edit.redo",
      group: "Edit",
      title: "Redo",
      shortcut: "Ctrl+Y",
      keywords: ["forward"],
      run: deps.redo,
    },
    {
      id: "edit.duplicate",
      group: "Edit",
      title: "Duplicate selected node",
      shortcut: "Ctrl+D",
      disabled: !hasSelection,
      run: () => {
        if (deps.selectedNodeId) deps.duplicate(deps.selectedNodeId);
      },
    },
    {
      id: "edit.delete",
      group: "Edit",
      title: "Delete selected node",
      shortcut: "Del",
      disabled: !hasSelection,
      run: () => {
        if (deps.selectedNodeId) deps.remove(deps.selectedNodeId);
      },
    },
    {
      id: "edit.clear-selection",
      group: "Edit",
      title: "Clear selection",
      shortcut: "Esc",
      run: deps.clearSelection,
    },
    {
      id: "view.fit",
      group: "View",
      title: "Fit view to graph",
      run: deps.fitView,
    },
    {
      id: "view.expand",
      group: "View",
      title: "Toggle expanded view",
      run: deps.toggleExpanded,
    },
    {
      id: "view.library",
      group: "View",
      title: "Toggle library panel",
      run: deps.toggleLibrary,
    },
    {
      id: "view.layer.schemas",
      group: "View",
      title: "Toggle schemas layer",
      shortcut: "Alt+1",
      run: () => deps.toggleLayer("schemas"),
    },
    {
      id: "view.layer.prompts",
      group: "View",
      title: "Toggle prompts layer",
      shortcut: "Alt+2",
      run: () => deps.toggleLayer("prompts"),
    },
    {
      id: "view.layer.vars",
      group: "View",
      title: "Toggle vars layer",
      shortcut: "Alt+3",
      run: () => deps.toggleLayer("vars"),
    },
    {
      id: "file.search-nodes",
      group: "File",
      title: "Search nodes on canvas",
      shortcut: "/",
      run: deps.openSearch,
    },
    {
      id: "file.open",
      group: "File",
      title: "Open file…",
      keywords: ["recents", "examples", "browse"],
      run: deps.openFilePicker,
    },
    {
      id: "nav.runs",
      group: "Navigate",
      title: "Runs",
      keywords: ["run console", "history"],
      run: () => deps.navigate("/runs"),
    },
    {
      id: "nav.board",
      group: "Navigate",
      title: "Board",
      keywords: ["kanban", "issues"],
      run: () => deps.navigate("/board"),
    },
    {
      id: "nav.pipelines",
      group: "Navigate",
      title: "Pipelines",
      keywords: ["pipeline boards", "human input", "bots"],
      run: () => deps.navigate("/pipelines"),
    },
    {
      id: "nav.dispatcher",
      group: "Navigate",
      title: "Dispatcher",
      keywords: ["dispatcher", "retries"],
      run: () => deps.navigate("/dispatcher"),
    },
    {
      id: "nav.home",
      group: "Navigate",
      title: "Home",
      run: () => deps.navigate("/"),
    },
  ];
}
