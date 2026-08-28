import { useEffect, useMemo, useRef, useState } from "react";
import { ReactFlowProvider } from "@xyflow/react";
import { useLocation, useSearch } from "wouter";
import { ChevronLeftIcon, ChevronRightIcon } from "@radix-ui/react-icons";

import Canvas from "@/components/Canvas/Canvas";
import Inspector from "@/components/Inspector/Inspector";
import Toolbar from "@/components/Toolbar/Toolbar";
import DiagnosticsPanel from "@/components/Diagnostics/DiagnosticsPanel";
import LibraryPanel from "@/components/Library/LibraryPanel";
import SubNodePalette from "@/components/Canvas/SubNodePalette";
import SourceView from "@/components/SourceView/SourceView";
import { DesktopOnlyNotice, IconButton } from "@/components/ui";
import { useUIStore } from "@/store/ui";
import { useDocumentStore, useDocumentStoreInstance } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import { useAutoValidation } from "@/hooks/useAutoValidation";
import { useAutoOpenDiagnosticsOnError } from "@/hooks/useAutoOpenDiagnosticsOnError";
import { useFileWatcher } from "@/hooks/useFileWatcher";
import { editorDeepLinkTargetsDocument } from "@/lib/editorDeepLink";
import {
  useAssistantPageContext,
  type AssistantPageContextContribution,
  type PageContextValue,
} from "@/lib/chatDock/pageContext";
import { findNodeDecl } from "@/lib/defaults";
import { useTabsStore } from "@/store/tabs";

interface EditorViewProps {
  // Whether this editor tab is currently the visible one. EditorTabsView
  // keeps inactive tabs mounted with display:none; the Canvas uses this
  // to refit its viewport when the tab regains visibility (React Flow
  // measured at 0×0 while hidden otherwise renders blank on return).
  // Defaults to true so a standalone EditorView (deep-link fallback,
  // tests) behaves exactly as before.
  active?: boolean;
}

export default function EditorView({ active = true }: EditorViewProps) {
  // The active per-tab document store (or the module default when this
  // view is mounted outside an EditorTabHost — e.g. a deep-linked
  // /editor route in the fallback Switch).
  const docStoreInst = useDocumentStoreInstance();
  const sourceViewOpen = useUIStore((s) => s.sourceViewOpen);
  const diagnosticsPanelOpen = useUIStore((s) => s.diagnosticsPanelOpen);
  const expanded = useUIStore((s) => s.expanded);
  const libraryExpanded = useUIStore((s) => s.libraryExpanded);
  const inSubNodeView = useUIStore((s) => s.subNodeViewStack.length > 0);
  const inspectorWidth = useUIStore((s) => s.inspectorWidth);
  const setInspectorWidth = useUIStore((s) => s.setInspectorWidth);
  const inspectorCollapsed = useUIStore((s) => s.inspectorCollapsed);
  const toggleInspectorCollapsed = useUIStore((s) => s.toggleInspectorCollapsed);
  const setPendingFitNodeId = useUIStore((s) => s.setPendingFitNodeId);
  const currentFilePath = useDocumentStore((s) => s.currentFilePath);
  const iterDocument = useDocumentStore((s) => s.document);
  const dirty = useDocumentStore(
    (s) => s._generation !== s._savedGeneration,
  );
  const diagnosticCount = useDocumentStore((s) => s.diagnostics.length);
  const warningCount = useDocumentStore((s) => s.warnings.length);
  const selectedNodeId = useSelectionStore((s) => s.selectedNodeId);
  const selectedEdgeId = useSelectionStore((s) => s.selectedEdgeId);
  const setSelectedNode = useSelectionStore((s) => s.setSelectedNode);
  const activeWorkflowName = useUIStore((s) => s.activeWorkflowName);
  const activeSidebarTab = useUIStore((s) => s.activeTab);
  const editingItem = useUIStore((s) => s.editingItem);
  const activeEditorTab = useTabsStore((s) =>
    s.tabs.find((tab) => tab.id === s.activeEditorTabId),
  );

  const assistantContext = useMemo<AssistantPageContextContribution>(() => {
    const selectedNode =
      iterDocument && selectedNodeId
        ? findNodeDecl(iterDocument, selectedNodeId)
        : null;
    const visibleItem = (() => {
      if (!iterDocument || !editingItem) return null;
      if (editingItem.kind === "prompt") {
        return iterDocument.prompts.find((item) => item.name === editingItem.name) ?? {
          kind: "prompt",
          name: editingItem.name,
        };
      }
      if (editingItem.kind === "schema") {
        return iterDocument.schemas.find((item) => item.name === editingItem.name) ?? {
          kind: "schema",
          name: editingItem.name,
        };
      }
      // Variable values may resolve to credentials. Naming the visible item
      // disambiguates "cette variable" without forwarding its value.
      return { kind: "var", name: editingItem.name };
    })();

    const selection: Record<string, PageContextValue> = {};
    if (selectedNode) {
      selection.node = {
        kind: selectedNode.kind,
        name: selectedNode.decl.name,
        configuration: selectedNode.decl as unknown as PageContextValue,
      };
    }
    if (selectedEdgeId) selection.edgeId = selectedEdgeId;
    if (visibleItem) {
      selection.editingItem = visibleItem as unknown as PageContextValue;
    }

    const file = currentFilePath ?? activeEditorTab?.params.file;
    const draftRunId = activeEditorTab?.params.draft;
    const entityId = file ?? (draftRunId ? `draft/${draftRunId}` : activeEditorTab?.id);
    const section = editingItem
      ? `${editingItem.kind}-editor`
      : selectedNode
        ? `${selectedNode.kind}-inspector`
        : selectedEdgeId
          ? "edge-inspector"
          : sourceViewOpen
            ? "canvas-and-source"
            : activeSidebarTab;

    return {
      title: activeEditorTab?.label || file || "Bot editor",
      section,
      ...(entityId
        ? {
            entity: {
              type: "bot",
              id: entityId,
              label: activeEditorTab?.label || file || "Untitled bot",
            },
          }
        : {}),
      state: {
        dirty,
        view: sourceViewOpen ? "canvas-and-source" : "canvas",
        ...(file ? { file } : {}),
        ...(draftRunId ? { draftRunId } : {}),
        ...(activeWorkflowName ? { activeWorkflow: activeWorkflowName } : {}),
        diagnosticCount,
        warningCount,
        ...(Object.keys(selection).length > 0 ? { selection } : {}),
      },
    };
  }, [
    iterDocument,
    selectedNodeId,
    selectedEdgeId,
    editingItem,
    currentFilePath,
    activeEditorTab,
    dirty,
    sourceViewOpen,
    activeSidebarTab,
    activeWorkflowName,
    diagnosticCount,
    warningCount,
  ]);
  useAssistantPageContext(assistantContext, active);

  const search = useSearch();
  const [, setLocation] = useLocation();
  const [bannerRunId, setBannerRunId] = useState<string | null>(null);
  // Track which `?file=...&node=...` deep-link we have already consumed
  // so a re-render (or a setLocation that strips the params) doesn't
  // reload the document or re-trigger fitView. Keyed on the exact
  // search string so a fresh "Open in editor" click always wins.
  const handledSearch = useRef<string | null>(null);

  useAutoValidation();
  useAutoOpenDiagnosticsOnError();
  useFileWatcher();

  // Honor node-focus deep links from the run console:
  // /editor?file=<workspace-relative>&node=<ir_node_id>&from=<runId>.
  // EditorTabsView + EditorTabHost exclusively own file/tab hydration.
  // Keeping that responsibility out of every mounted EditorView prevents a
  // hidden or untitled tab from consuming another tab's global ?file= URL.
  useEffect(() => {
    if (handledSearch.current === search) return;
    const params = new URLSearchParams(search);
    const file = params.get("file");
    const node = params.get("node");
    const from = params.get("from");
    if (!file && !node && !from) {
      handledSearch.current = search;
      return;
    }

    // The matching EditorTabHost may still be loading. Do not mark this
    // search as handled until the active document is the requested file.
    if (!editorDeepLinkTargetsDocument(active, currentFilePath, file)) return;
    handledSearch.current = search;

    if (node) {
      setSelectedNode(node);
      setPendingFitNodeId(node);
    }
    if (from) setBannerRunId(from);
  }, [
    active,
    search,
    currentFilePath,
    setPendingFitNodeId,
    setSelectedNode,
  ]);

  const dismissBanner = () => {
    setBannerRunId(null);
    // Strip the query params so refresh / share-link doesn't re-trigger
    // the deep-link logic (and shows a clean URL to the user).
    setLocation("/editor", { replace: true });
  };

  useEffect(() => {
    const handler = (e: BeforeUnloadEvent) => {
      if (docStoreInst.getState().isDirty()) {
        e.preventDefault();
      }
    };
    window.addEventListener("beforeunload", handler);
    return () => window.removeEventListener("beforeunload", handler);
  }, []);

  const draggingRef = useRef(false);
  const [draftWidth, setDraftWidth] = useState<number | null>(null);

  useEffect(() => {
    const onMove = (e: MouseEvent) => {
      if (!draggingRef.current) return;
      setDraftWidth(window.innerWidth - e.clientX);
    };
    const onUp = () => {
      if (!draggingRef.current) return;
      draggingRef.current = false;
      document.body.style.cursor = "";
      document.body.style.userSelect = "";
      setDraftWidth((current) => {
        if (current !== null) setInspectorWidth(current);
        return null;
      });
    };
    window.addEventListener("mousemove", onMove);
    window.addEventListener("mouseup", onUp);
    return () => {
      window.removeEventListener("mousemove", onMove);
      window.removeEventListener("mouseup", onUp);
    };
  }, [setInspectorWidth]);

  const startResize = (e: React.MouseEvent) => {
    e.preventDefault();
    draggingRef.current = true;
    document.body.style.cursor = "col-resize";
    document.body.style.userSelect = "none";
  };

  const COLLAPSED_INSPECTOR_PX = 28;
  const effectiveInspectorWidth = inspectorCollapsed
    ? COLLAPSED_INSPECTOR_PX
    : (draftWidth ?? inspectorWidth);
  const leftWidth = libraryExpanded || inSubNodeView ? 280 : 64;

  return (
    <ReactFlowProvider>
      <div className="h-full w-full overflow-hidden flex flex-col">
        {bannerRunId && (
          <div className="flex items-center gap-2 px-3 py-1.5 text-xs bg-accent-soft text-accent-fg border-b border-border-default">
            <span aria-hidden>↗</span>
            <span>
              Opened from run{" "}
              <code className="font-mono text-micro bg-surface-2 px-1 py-0.5 rounded">
                {bannerRunId.length > 12 ? `${bannerRunId.slice(0, 8)}…` : bannerRunId}
              </code>
            </span>
            <button
              type="button"
              onClick={() => setLocation(`/runs/${encodeURIComponent(bannerRunId)}`)}
              className="ml-1 px-1.5 py-0.5 rounded hover:bg-surface-2"
              title="Back to the run console"
            >
              ← Back to run
            </button>
            <button
              type="button"
              onClick={dismissBanner}
              className="ml-auto px-1.5 py-0.5 rounded hover:bg-surface-2"
              aria-label="Dismiss"
              title="Dismiss"
            >
              ✕
            </button>
          </div>
        )}
      <DesktopOnlyNotice feature="the workflow editor" lsKey="iterion.editor.mobile-optin">
      <div
        className="flex-1 min-h-0 grid transition-[grid-template-columns] duration-200 outline-none"
        style={
          expanded
            ? { gridTemplateColumns: "1fr", gridTemplateRows: "1fr" }
            : {
                gridTemplateColumns: `${leftWidth}px 1fr ${effectiveInspectorWidth}px`,
                gridTemplateRows: `40px 1fr ${diagnosticsPanelOpen ? "160px" : "0px"}`,
              }
        }
      >
        {!expanded && (
          <div className="col-span-3">
            <Toolbar />
          </div>
        )}

        {!expanded && (
          <div className="border-r border-border-default overflow-y-auto">
            {inSubNodeView ? <SubNodePalette /> : <LibraryPanel />}
          </div>
        )}

        <div className="min-h-0 flex">
          <div className={sourceViewOpen && !expanded ? "w-1/2 h-full" : "w-full h-full"}>
            <Canvas active={active} />
          </div>
          {sourceViewOpen && !expanded && (
            <div className="w-1/2 h-full border-l border-border-default">
              <SourceView />
            </div>
          )}
        </div>

        {!expanded && (
          <div className="relative border-l border-border-default min-h-0 flex flex-col overflow-hidden">
            {inspectorCollapsed ? (
              <IconButton
                label="Show inspector"
                size="sm"
                variant="ghost"
                className="mt-2 mx-auto"
                onClick={toggleInspectorCollapsed}
              >
                <ChevronLeftIcon />
              </IconButton>
            ) : (
              <>
                <div
                  role="separator"
                  aria-orientation="vertical"
                  aria-label="Resize inspector"
                  onMouseDown={startResize}
                  className="absolute left-0 top-0 bottom-0 w-1 -translate-x-1/2 cursor-col-resize hover:bg-accent/50 z-[var(--z-canvas)]"
                  title="Drag to resize"
                />
                <div className="flex items-center justify-end px-1 py-0.5 border-b border-border-default shrink-0 bg-surface-1">
                  <IconButton
                    label="Hide inspector"
                    size="sm"
                    variant="ghost"
                    onClick={toggleInspectorCollapsed}
                  >
                    <ChevronRightIcon />
                  </IconButton>
                </div>
                <div className="flex-1 min-h-0 overflow-hidden">
                  <Inspector />
                </div>
              </>
            )}
          </div>
        )}

        {!expanded && (
          <div
            className={`col-span-3 border-t border-border-default ${
              diagnosticsPanelOpen ? "overflow-y-auto" : "overflow-hidden"
            }`}
          >
            <DiagnosticsPanel />
          </div>
        )}
      </div>
      </DesktopOnlyNotice>
      </div>
    </ReactFlowProvider>
  );
}
