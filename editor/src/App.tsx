import { useEffect } from "react";
import { ReactFlowProvider } from "@xyflow/react";
import Canvas from "./components/Canvas/Canvas";
import SidebarTabs from "./components/Panels/SidebarTabs";
import Toolbar from "./components/Toolbar/Toolbar";
import DiagnosticsPanel from "./components/Diagnostics/DiagnosticsPanel";
import LibraryPanel from "./components/Library/LibraryPanel";
import SourceView from "./components/SourceView/SourceView";
import ToastContainer from "./components/shared/Toast";
import EditItemModal from "./components/Modals/EditItemModal";
import { useUIStore } from "./store/ui";
import { useDocumentStore } from "./store/document";
import { useAutoValidation } from "./hooks/useAutoValidation";
import { useFileWatcher } from "./hooks/useFileWatcher";

export default function App() {
  const sourceViewOpen = useUIStore((s) => s.sourceViewOpen);
  const diagnosticsPanelOpen = useUIStore((s) => s.diagnosticsPanelOpen);
  const expanded = useUIStore((s) => s.expanded);
  const libraryExpanded = useUIStore((s) => s.libraryExpanded);
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

  return (
    <ReactFlowProvider>
      <div className={`h-screen w-screen grid bg-gray-900 text-white transition-[grid-template-columns] duration-200 ${
        expanded
          ? "grid-rows-[1fr] grid-cols-[1fr]"
          : `${libraryExpanded ? "grid-cols-[280px_1fr_320px]" : "grid-cols-[64px_1fr_320px]"} ${
              diagnosticsPanelOpen ? "grid-rows-[48px_1fr_160px]" : "grid-rows-[48px_1fr_0px]"
            }`
      }`}>
        {/* Toolbar spans full width */}
        {!expanded && (
          <div className="col-span-3 border-b border-gray-700">
            <Toolbar />
          </div>
        )}

        {/* Left: library panel */}
        {!expanded && (
          <div className="border-r border-gray-700 overflow-y-auto">
            <LibraryPanel />
          </div>
        )}

        {/* Center: canvas + optional source view */}
        <div className="min-h-0 flex">
          <div className={sourceViewOpen && !expanded ? "w-1/2 h-full" : "w-full h-full"}>
            <Canvas />
          </div>
          {sourceViewOpen && !expanded && (
            <div className="w-1/2 h-full border-l border-gray-700">
              <SourceView />
            </div>
          )}
        </div>

        {/* Right sidebar: tabbed panels */}
        {!expanded && (
          <div className="border-l border-gray-700 overflow-y-auto">
            <SidebarTabs />
          </div>
        )}

        {/* Bottom diagnostics spans full width */}
        {!expanded && (
          <div className={`col-span-3 border-t border-gray-700 ${diagnosticsPanelOpen ? "overflow-y-auto" : "overflow-hidden"}`}>
            <DiagnosticsPanel />
          </div>
        )}
      </div>
      <EditItemModal />
      <ToastContainer />
    </ReactFlowProvider>
  );
}
