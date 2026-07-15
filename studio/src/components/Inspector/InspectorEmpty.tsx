import type { ReactNode } from "react";

import { useUIStore, type SidebarTab } from "@/store/ui";
import SchemaEditor from "@/components/Panels/SchemaEditor";
import PromptEditor from "@/components/Panels/PromptEditor";
import VarsEditor from "@/components/Panels/VarsEditor";
import WorkflowSettingsForm from "@/components/Panels/WorkflowSettingsForm";
import CommentsEditor from "@/components/Panels/CommentsEditor";
import MCPServersEditor from "@/components/Panels/MCPServersEditor";
import BotMetadataForm from "@/components/Panels/BotMetadataForm";
import { useBotForOpenFile } from "@/hooks/useBotForOpenFile";
import { Tabs } from "@/components/ui";

// `comments` keeps the compact "##" glyph (the DSL comment sigil) so the
// tab row doesn't overflow the inspector's default width, but carries a
// title tooltip so the label isn't a mystery on first encounter. The other
// tabs are short enough to spell out.
const BASE_TABS: { value: SidebarTab; label: ReactNode }[] = [
  { value: "workflow", label: "Workflow" },
  { value: "vars", label: "Vars" },
  { value: "schemas", label: "Schemas" },
  { value: "prompts", label: "Prompts" },
  { value: "mcp", label: "MCP" },
  { value: "comments", label: <span title="Comments (document-level ## notes)">##</span> },
];

/**
 * Rendered by the Inspector when nothing is selected — shows the document-level
 * editing surfaces (workflow settings, vars, schemas, prompts, comments). When
 * the open file is a bundle's main.bot, a "Bot" tab is appended for editing the
 * bundle's manifest metadata (persona, description, when-to-use, triggers,
 * catalog toggle).
 *
 * Note: the legacy "properties" tab is intentionally absent; selection-driven
 * editing is now handled by the Inspector itself.
 */
export default function InspectorEmpty() {
  const activeTab = useUIStore((s) => s.activeTab);
  const setActiveTab = useUIStore((s) => s.setActiveTab);
  const bot = useBotForOpenFile();

  const tabs = bot ? [...BASE_TABS, { value: "bot" as SidebarTab, label: "Bot" }] : BASE_TABS;

  // Coerce legacy "properties" persistence — and the "bot" tab when the
  // open file isn't a bundle — to a sensible default.
  const tab: SidebarTab =
    activeTab === "properties" || (activeTab === "bot" && !bot) ? "workflow" : activeTab;

  return (
    <Tabs
      value={tab}
      onValueChange={(v) => setActiveTab(v as SidebarTab)}
      items={tabs.map((t) => ({ value: t.value, label: t.label }))}
      panels={{
        workflow: <WorkflowSettingsForm />,
        vars: <VarsEditor />,
        schemas: <SchemaEditor />,
        prompts: <PromptEditor />,
        mcp: <MCPServersEditor />,
        comments: <CommentsEditor />,
        ...(bot ? { bot: <BotMetadataForm key={bot.name} bot={bot} /> } : {}),
      }}
      className="h-full"
      // overflow-x-auto so the row stays reachable when a bundle's "Bot"
      // tab pushes the tab count to 7 — without it the last tab(s) were
      // clipped at the inspector's default width. The scrollbar itself is
      // hidden (scrollbar-width:none + ::-webkit-scrollbar) so the strip
      // scrolls without reserving a permanent scrollbar row; keyboard
      // focus / wheel / drag still scroll it.
      listClassName="shrink-0 px-2 overflow-x-auto [scrollbar-width:none] [&::-webkit-scrollbar]:hidden"
    />
  );
}
