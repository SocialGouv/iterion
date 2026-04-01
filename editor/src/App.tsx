import { ReactFlowProvider } from "@xyflow/react";
import Canvas from "./components/Canvas/Canvas";
import SidebarTabs from "./components/Panels/SidebarTabs";
import Toolbar from "./components/Toolbar/Toolbar";
import DiagnosticsPanel from "./components/Diagnostics/DiagnosticsPanel";
import NodePalette from "./components/Palette/NodePalette";
import SourceView from "./components/SourceView/SourceView";
import { useUIStore } from "./store/ui";
import { useAutoValidation } from "./hooks/useAutoValidation";

export default function App() {
  const sourceViewOpen = useUIStore((s) => s.sourceViewOpen);
  useAutoValidation();

  return (
    <ReactFlowProvider>
      <div className="h-screen w-screen grid grid-rows-[48px_1fr_160px] grid-cols-[64px_1fr_320px] bg-gray-900 text-white">
        {/* Toolbar spans full width */}
        <div className="col-span-3 border-b border-gray-700">
          <Toolbar />
        </div>

        {/* Left palette */}
        <div className="border-r border-gray-700 overflow-y-auto">
          <NodePalette />
        </div>

        {/* Center: canvas + optional source view */}
        <div className="min-h-0 flex">
          <div className={sourceViewOpen ? "w-1/2 h-full" : "w-full h-full"}>
            <Canvas />
          </div>
          {sourceViewOpen && (
            <div className="w-1/2 h-full border-l border-gray-700">
              <SourceView />
            </div>
          )}
        </div>

        {/* Right sidebar: tabbed panels */}
        <div className="border-l border-gray-700 overflow-y-auto">
          <SidebarTabs />
        </div>

        {/* Bottom diagnostics spans full width */}
        <div className="col-span-3 border-t border-gray-700 overflow-y-auto">
          <DiagnosticsPanel />
        </div>
      </div>
    </ReactFlowProvider>
  );
}
