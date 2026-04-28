import { useEffect, useRef, useState } from "react";
import { ReactFlowProvider } from "@xyflow/react";
import Canvas from "./components/Canvas/Canvas";
import Inspector from "./components/Inspector/Inspector";
import Toolbar from "./components/Toolbar/Toolbar";
import DiagnosticsPanel from "./components/Diagnostics/DiagnosticsPanel";
import LibraryPanel from "./components/Library/LibraryPanel";
import SubNodePalette from "./components/Canvas/SubNodePalette";
import SourceView from "./components/SourceView/SourceView";
import ToastContainer from "./components/shared/Toast";
import { useUIStore } from "./store/ui";
import { useDocumentStore } from "./store/document";
import { useAutoValidation } from "./hooks/useAutoValidation";
import { useFileWatcher } from "./hooks/useFileWatcher";

export default function App() {
  const sourceViewOpen = useUIStore((s) => s.sourceViewOpen);
  const diagnosticsPanelOpen = useUIStore((s) => s.diagnosticsPanelOpen);
  const expanded = useUIStore((s) => s.expanded);
  const libraryExpanded = useUIStore((s) => s.libraryExpanded);
  const inSubNodeView = useUIStore((s) => s.subNodeViewStack.length > 0);
  const inspectorWidth = useUIStore((s) => s.inspectorWidth);
  const setInspectorWidth = useUIStore((s) => s.setInspectorWidth);
  useAutoValidation();
  useFileWatcher();

  // Warn before closing with unsaved changes
  useEffect(() => {
    const handler = (e: BeforeUnloadEvent) => {
      if (useDocumentStore.getState().isDirty()) {
        e.preventDefault();
      }
    };
    window.addEventListener("beforeunload", handler);
    return () => window.removeEventListener("beforeunload", handler);
  }, []);

  // Inspector resize: track drag state and a transient draftWidth so the
  // grid-template-columns updates smoothly without thrashing the store.
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

  const effectiveInspectorWidth = draftWidth ?? inspectorWidth;
  const leftWidth = libraryExpanded || inSubNodeView ? 280 : 64;

  return (
    <ReactFlowProvider>
      <div
        className="h-screen w-screen grid bg-surface-0 text-fg-default transition-[grid-template-columns] duration-200"
        style={
          expanded
            ? { gridTemplateColumns: "1fr", gridTemplateRows: "1fr" }
            : {
                gridTemplateColumns: `${leftWidth}px 1fr ${effectiveInspectorWidth}px`,
                gridTemplateRows: `48px 1fr ${diagnosticsPanelOpen ? "160px" : "0px"}`,
              }
        }
      >
        {/* Toolbar spans full width */}
        {!expanded && (
          <div className="col-span-3 border-b border-border-default">
            <Toolbar />
          </div>
        )}

        {/* Left: library panel or subnode palette */}
        {!expanded && (
          <div className="border-r border-border-default overflow-y-auto">
            {inSubNodeView ? <SubNodePalette /> : <LibraryPanel />}
          </div>
        )}

        {/* Center: canvas + optional source view */}
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

        {/* Right sidebar: contextual Inspector with drag handle on its left edge */}
        {!expanded && (
          <div className="relative border-l border-border-default min-h-0 flex flex-col overflow-hidden">
            <div
              role="separator"
              aria-orientation="vertical"
              aria-label="Resize inspector"
              onMouseDown={startResize}
              className="absolute left-0 top-0 bottom-0 w-1 -translate-x-1/2 cursor-col-resize hover:bg-accent/50 z-10"
              title="Drag to resize"
            />
            <Inspector />
          </div>
        )}

        {/* Bottom diagnostics spans full width */}
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
      <ToastContainer />
    </ReactFlowProvider>
  );
}
