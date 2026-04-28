import { useUIStore, type SidebarTab } from "@/store/ui";
import SchemaEditor from "@/components/Panels/SchemaEditor";
import PromptEditor from "@/components/Panels/PromptEditor";
import VarsEditor from "@/components/Panels/VarsEditor";
import WorkflowSettingsForm from "@/components/Panels/WorkflowSettingsForm";
import CommentsEditor from "@/components/Panels/CommentsEditor";
import { Tabs } from "@/components/ui";

const TABS: { value: SidebarTab; label: string }[] = [
  { value: "workflow", label: "Workflow" },
  { value: "vars", label: "Vars" },
  { value: "schemas", label: "Schemas" },
  { value: "prompts", label: "Prompts" },
  { value: "comments", label: "##" },
];

/**
 * Rendered by the Inspector when nothing is selected — shows the document-level
 * editing surfaces (workflow settings, vars, schemas, prompts, comments).
 *
 * Note: the legacy "properties" tab is intentionally absent; selection-driven
 * editing is now handled by the Inspector itself.
 */
export default function InspectorEmpty() {
  const activeTab = useUIStore((s) => s.activeTab);
  const setActiveTab = useUIStore((s) => s.setActiveTab);

  // Coerce legacy "properties" persistence to a sensible default.
  const tab: SidebarTab =
    activeTab === "properties" ? "workflow" : activeTab;

  return (
    <Tabs
      value={tab}
      onValueChange={(v) => setActiveTab(v as SidebarTab)}
      items={TABS.map((t) => ({ value: t.value, label: t.label }))}
      panels={{
        workflow: <WorkflowSettingsForm />,
        vars: <VarsEditor />,
        schemas: <SchemaEditor />,
        prompts: <PromptEditor />,
        comments: <CommentsEditor />,
      }}
      className="h-full"
      listClassName="shrink-0 px-2"
    />
  );
}
