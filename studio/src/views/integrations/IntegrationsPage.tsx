import { useLocation, useSearch } from "wouter";

import { Tabs } from "@/components/ui/Tabs";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { useAuth } from "@/auth/AuthContext";
import { useCanManageTeam } from "@/hooks/useCanManageTeam";
import { CloudOnlyNotice } from "@/components/shared/CloudOnlyNotice";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import { useServerInfoStore } from "@/store/serverInfo";

import IntegrationsTab from "@/views/teams/tabs/IntegrationsTab";
import WebhooksTab from "@/views/teams/tabs/WebhooksTab";
import SecretsTab from "@/views/teams/tabs/SecretsTab";
import BindingsTab from "@/views/teams/tabs/BindingsTab";
import ModelProvidersTab from "./ModelProvidersTab";
import ProjectBoardTab from "./ProjectBoardTab";

// FORGES_LABEL is the user-facing name of the forge-connection tab. The
// internal id stays `forges` so deep links keep working; the label
// matches the "Repositories" section heading and the wizard CTA so the
// operator navigates one vocabulary instead of three.
const FORGES_LABEL = "Repositories";

// Integrations is its own top-level destination (/integrations), distinct from
// "Team settings" (/teams/:id, reached from the account chip). Everything here
// is about connecting iterion to the outside world — git forges, the webhooks
// they fire, the secrets bots consume, and the LLM subscriptions they run on.
type Tab = "forges" | "webhooks" | "secrets" | "bindings" | "providers" | "project-board";

const TABS: Array<{ id: Tab; label: string }> = [
  { id: "forges", label: FORGES_LABEL },
  { id: "webhooks", label: "Webhooks" },
  { id: "secrets", label: "Secrets" },
  { id: "bindings", label: "Bot bindings" },
  { id: "providers", label: "Model providers" },
  { id: "project-board", label: "Project board" },
];

const TAB_ITEMS = TABS.map((t) => ({ value: t.id, label: t.label }));

export default function IntegrationsPage() {
  const { activeTeam } = useAuth();
  const canManage = useCanManageTeam();
  // Integrations are team-scoped cloud resources (forge/webhook/secret
  // stores are wired only in cloud mode). Local mode has no team selector,
  // so "select a team" would be an instruction that can't be followed —
  // show the standard cloud-only state instead.
  const serverInfo = useServerInfoStore((s) => s.info);
  const isCloud = serverInfo?.mode === "cloud";
  const search = useSearch();
  const [, navigate] = useLocation();

  // tab is derived straight from ?tab= (the URL is the source of truth), so a
  // deep link (e.g. the catalog's "Connect to a repo" → ?tab=forges&bot=…)
  // selects the right tab with no state to keep in sync. selectTab just
  // rewrites the URL; the re-render picks up the new tab.
  const tabParam = new URLSearchParams(search).get("tab");
  const tab: Tab = TABS.some((x) => x.id === tabParam) ? (tabParam as Tab) : "forges";
  const selectTab = (t: Tab) => navigate(`/integrations?tab=${t}`, { replace: true });

  const teamID = activeTeam?.team_id;

  useHeaderSlot({
    left: <span className="text-sm font-semibold">Integrations</span>,
    right: activeTeam ? (
      <span className="text-xs text-fg-muted">{activeTeam.team_name}</span>
    ) : null,
  });

  if (serverInfo && !isCloud) {
    return (
      <div className="h-full overflow-auto">
        <div className="max-w-6xl mx-auto p-3 sm:p-6">
          <CloudOnlyNotice
            title="Integrations"
            feature="Team integration management (repositories, webhooks, secrets, model providers)"
          />
        </div>
      </div>
    );
  }

  if (!teamID) {
    return (
      <div className="h-full overflow-auto">
        <div className="max-w-6xl mx-auto p-3 sm:p-6">
          <InlineBanner tone="info" layout="inline">
            Select a team to manage its integrations. Integrations (repositories, webhooks,
            secrets, model providers) are team-scoped.
          </InlineBanner>
        </div>
      </div>
    );
  }

  return (
    <div className="h-full overflow-auto">
      <div className="max-w-6xl mx-auto p-3 sm:p-6 grid grid-cols-1 sm:grid-cols-[200px_1fr] gap-4 sm:gap-6">
        <Tabs
          variant="pill"
          value={tab}
          onValueChange={(v) => selectTab(v as Tab)}
          items={TAB_ITEMS}
          listClassName="flex sm:flex-col gap-1 flex-wrap"
          triggerClassName="sm:w-full sm:text-left"
        />

        <main>
          {tab === "forges" && <IntegrationsTab teamID={teamID} canManage={canManage} />}
          {tab === "webhooks" && <WebhooksTab teamID={teamID} canManage={canManage} />}
          {tab === "secrets" && <SecretsTab teamID={teamID} canManage={canManage} />}
          {tab === "bindings" && <BindingsTab teamID={teamID} canManage={canManage} />}
          {tab === "providers" && <ModelProvidersTab teamID={teamID} />}
          {tab === "project-board" && (
            <ProjectBoardTab teamID={teamID} canManage={canManage} />
          )}
        </main>
      </div>
    </div>
  );
}
