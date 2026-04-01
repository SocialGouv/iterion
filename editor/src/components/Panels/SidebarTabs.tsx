import { useEffect } from "react";
import { useUIStore, type SidebarTab } from "@/store/ui";
import { useSelectionStore } from "@/store/selection";
import PropertiesPanel from "./PropertiesPanel";
import SchemaEditor from "./SchemaEditor";
import PromptEditor from "./PromptEditor";
import VarsEditor from "./VarsEditor";
import WorkflowSettingsForm from "./WorkflowSettingsForm";
import CommentsEditor from "./CommentsEditor";

const TABS: { id: SidebarTab; label: string }[] = [
  { id: "properties", label: "Props" },
  { id: "schemas", label: "Schemas" },
  { id: "prompts", label: "Prompts" },
  { id: "vars", label: "Vars" },
  { id: "workflow", label: "Workflow" },
  { id: "comments", label: "##" },
];

export default function SidebarTabs() {
  const activeTab = useUIStore((s) => s.activeTab);
  const setActiveTab = useUIStore((s) => s.setActiveTab);
  const selectedNodeId = useSelectionStore((s) => s.selectedNodeId);
  const selectedEdgeId = useSelectionStore((s) => s.selectedEdgeId);

  // Auto-switch to Properties tab when a node or edge is selected,
  // but only from passive tabs to avoid disrupting active editing
  useEffect(() => {
    if ((selectedNodeId || selectedEdgeId) && (activeTab === "workflow" || activeTab === "comments")) {
      setActiveTab("properties");
    }
  }, [selectedNodeId, selectedEdgeId, activeTab, setActiveTab]);

  const hasSelection = !!(selectedNodeId || selectedEdgeId);

  return (
    <div className="h-full flex flex-col">
      <div className="flex border-b border-gray-700 shrink-0">
        {TABS.map((tab) => (
          <button
            key={tab.id}
            className={`flex-1 text-xs py-2 transition-colors ${
              activeTab === tab.id
                ? "text-white border-b-2 border-blue-500 bg-gray-800"
                : "text-gray-500 hover:text-gray-300"
            }`}
            onClick={() => setActiveTab(tab.id)}
          >
            {tab.label}
            {tab.id === "properties" && hasSelection && activeTab !== "properties" && (
              <span className="w-1.5 h-1.5 bg-blue-500 rounded-full inline-block ml-1" />
            )}
          </button>
        ))}
      </div>
      <div className="flex-1 overflow-y-auto">
        {activeTab === "properties" && <PropertiesPanel />}
        {activeTab === "schemas" && <SchemaEditor />}
        {activeTab === "prompts" && <PromptEditor />}
        {activeTab === "vars" && <VarsEditor />}
        {activeTab === "workflow" && <WorkflowSettingsForm />}
        {activeTab === "comments" && <CommentsEditor />}
      </div>
    </div>
  );
}
