import { useEffect, useRef, useState } from "react";
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
import { IconButton } from "@/components/ui";
import { useUIStore } from "@/store/ui";
import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import { useAutoValidation } from "@/hooks/useAutoValidation";
import { useFileWatcher } from "@/hooks/useFileWatcher";
import * as api from "@/api/client";

export default function EditorView() {
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
  const addToast = useUIStore((s) => s.addToast);
  const setSelectedNode = useSelectionStore((s) => s.setSelectedNode);

  const search = useSearch();
  const [, setLocation] = useLocation();
  const [bannerRunId, setBannerRunId] = useState<string | null>(null);
  // Track which `?file=...&node=...` deep-link we have already consumed
  // so a re-render (or a setLocation that strips the params) doesn't
  // reload the document or re-trigger fitView. Keyed on the exact
  // search string so a fresh "Open in editor" click always wins.
  const handledSearch = useRef<string | null>(null);

  useAutoValidation();
  useFileWatcher();

  // Honor deep links from the run console: /?file=<workspace-relative>&node=<ir_node_id>&from=<runId>.
  // Mounted once, runs whenever the search string changes — the early
  // return avoids re-loading the same file on unrelated navigation.
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
    handledSearch.current = search;

    const docStore = useDocumentStore.getState();
    const alreadyOpen = file && docStore.currentFilePath === file;

    const applyNodeFocus = () => {
      if (!node) return;
      setSelectedNode(node);
      setPendingFitNodeId(node);
    };

    if (file && !alreadyOpen) {
      if (docStore.isDirty() && !window.confirm("You have unsaved changes. Discard them?")) {
        return;
      }
      api
        .openFile(file)
        .then((result) => {
          const ds = useDocumentStore.getState();
          ds.setDocument(result.document);
          ds.setCurrentFilePath(result.path);
          ds.markSaved();
          // Selection waits a tick so the new document's nodes have
          // been registered with React Flow before we attempt to
          // center on one of them.
          setTimeout(applyNodeFocus, 0);
        })
        .catch((err) => {
          addToast(`Open from run failed: ${(err as Error).message}`, "error");
        });
    } else {
      applyNodeFocus();
    }

    if (from) setBannerRunId(from);
  }, [search, addToast, setPendingFitNodeId, setSelectedNode]);

  const dismissBanner = () => {
    setBannerRunId(null);
    // Strip the query params so refresh / share-link doesn't re-trigger
    // the deep-link logic (and shows a clean URL to the user).
    setLocation("/", { replace: true });
  };

  useEffect(() => {
    const handler = (e: BeforeUnloadEvent) => {
      if (useDocumentStore.getState().isDirty()) {
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
      <div className="h-screen w-screen overflow-hidden flex flex-col bg-surface-0 text-fg-default">
        {bannerRunId && (
          <div className="flex items-center gap-2 px-3 py-1.5 text-xs bg-accent-soft text-accent-fg border-b border-border-default">
            <span aria-hidden>↗</span>
            <span>
              Opened from run{" "}
              <code className="font-mono text-[11px] bg-surface-2 px-1 py-0.5 rounded">
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
      <div
        className="flex-1 min-h-0 grid transition-[grid-template-columns] duration-200"
        style={
          expanded
            ? { gridTemplateColumns: "1fr", gridTemplateRows: "1fr" }
            : {
                gridTemplateColumns: `${leftWidth}px 1fr ${effectiveInspectorWidth}px`,
                gridTemplateRows: `48px 1fr ${diagnosticsPanelOpen ? "160px" : "0px"}`,
              }
        }
      >
        {!expanded && (
          <div className="col-span-3 border-b border-border-default">
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
            <Canvas />
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
                  className="absolute left-0 top-0 bottom-0 w-1 -translate-x-1/2 cursor-col-resize hover:bg-accent/50 z-10"
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
      </div>
    </ReactFlowProvider>
  );
}
