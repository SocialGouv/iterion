import { useCallback, useEffect, useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useLocation, useSearch } from "wouter";

import { useActiveRepo } from "@/hooks/useActiveRepo";
import { useCanManageTeam } from "@/hooks/useCanManageTeam";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import { Button } from "@/components/ui/Button";
import { EmptyState } from "@/components/ui/EmptyState";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Spinner } from "@/components/ui/Spinner";
import { Tabs } from "@/components/ui/Tabs";
import { errorMessage } from "@/lib/errorHints";
import { useServerInfoStore } from "@/store/serverInfo";
import {
  listTriggers,
  FeatureUnavailableError,
  type TriggerSubscription,
} from "@/api/triggers";
import NewTriggerDialog from "./NewTriggerDialog";
import SchedulesTab from "./SchedulesTab";
import TriggerFamiliesExplainer from "./TriggerFamiliesExplainer";
import TriggerList from "./TriggerList";

type Tab = "automations" | "schedules";

const TABS: Array<{ id: Tab; label: string }> = [
  { id: "automations", label: "Automations" },
  { id: "schedules", label: "Schedules" },
];

export default function TriggersView() {
  const [creatingTrigger, setCreatingTrigger] = useState(false);
  const [creatingSchedule, setCreatingSchedule] = useState(false);
  const search = useSearch();
  const [, navigate] = useLocation();
  const cloud = useServerInfoStore((s) => s.info?.mode === "cloud");

  // URL-synced tab (+ optional ?repo= schedules pre-filter), so
  // /triggers?tab=schedules&repo=owner/repo deep-links from Integrations.
  // With no trigger store, the Schedules tab is the one that works —
  // make it the landing tab instead of an "unavailable" panel.
  const defaultTab: Tab = useServerInfoStore((s) =>
    s.info?.triggers_enabled ? "automations" : "schedules",
  );
  const tabFromURL = (s: string): Tab => {
    const t = new URLSearchParams(s).get("tab");
    return TABS.some((x) => x.id === t) ? (t as Tab) : defaultTab;
  };
  const [tab, setTab] = useState<Tab>(() => tabFromURL(search));
  useEffect(() => {
    const t = tabFromURL(search);
    setTab((cur) => (cur === t ? cur : t));
  }, [search]);
  const selectTab = (t: Tab) => {
    setTab(t);
    navigate(t === defaultTab ? "/triggers" : `/triggers?tab=${t}`, { replace: true });
  };
  const repoParam = useMemo(() => new URLSearchParams(search).get("repo"), [search]);

  // Repo-first scope: the active repo narrows the list server-side
  // (ListByRepo also returns tenant-wide rows with no repo binding).
  const { activeRepo, overview, enabled: repoScope } = useActiveRepo();
  const scopeRepo = repoScope && !overview ? (activeRepo?.repo_full_name ?? "") : "";

  // The header "New schedule" button only renders when SchedulesTab can
  // actually open its dialog: cloud team context (repoScope), manage
  // rights, and a schedule store present (SchedulesTab reports absence
  // up via onUnavailable). Otherwise the click would be a silent no-op.
  const canManage = useCanManageTeam();
  const [schedUnavailable, setSchedUnavailable] = useState(false);
  // Stable identity: SchedulesTab keys its unavailable-report effect on
  // this callback.
  const onSchedUnavailable = useCallback(() => setSchedUnavailable(true), []);
  const canCreateSchedule = repoScope && canManage && !schedUnavailable;

  const triggersEnabled = useServerInfoStore((s) => s.info?.triggers_enabled ?? false);
  const queryClient = useQueryClient();
  // Gated on the server advertising a trigger store, skipping the
  // round-trip (and its console 404) otherwise — the Automations panel
  // shows its unavailable state, the Schedules tab keeps working. The
  // unscoped fetch shares the ["triggers"] cache with the bots gallery.
  const subsQuery = useQuery<TriggerSubscription[]>({
    queryKey: scopeRepo ? ["triggers", scopeRepo] : ["triggers"],
    queryFn: () => listTriggers(scopeRepo ? { repo: scopeRepo } : undefined),
    enabled: triggersEnabled,
    // The repo switch changes the key; keep the previous list on screen
    // while the new scope loads instead of flashing the spinner.
    placeholderData: (prev: TriggerSubscription[] | undefined) => prev,
  });
  const subs = subsQuery.data ?? null;
  const unavailable =
    !triggersEnabled || subsQuery.error instanceof FeatureUnavailableError;
  const loadErr =
    subsQuery.error && !(subsQuery.error instanceof FeatureUnavailableError)
      ? errorMessage(subsQuery.error)
      : null;
  const reload = useCallback(
    () => queryClient.invalidateQueries({ queryKey: ["triggers"] }),
    [queryClient],
  );

  useHeaderSlot({
    left: (
      <span className="text-xs font-medium text-fg-default">
        Automations
        {scopeRepo && tab === "automations" && (
          <span className="ml-2 rounded bg-surface-2 px-1.5 py-0.5 text-caption font-normal text-fg-muted">
            {scopeRepo}
          </span>
        )}
      </span>
    ),
    right:
      tab === "schedules" ? (
        canCreateSchedule ? (
          <Button variant="primary" size="sm" onClick={() => setCreatingSchedule(true)}>
            New schedule
          </Button>
        ) : null
      ) : (
        <Button variant="primary" size="sm" onClick={() => setCreatingTrigger(true)}>
          New trigger
        </Button>
      ),
  });

  const rows = useMemo(() => subs ?? [], [subs]);

  // The event-trigger store being absent doesn't hide the Schedules tab —
  // cloud servers carry schedules even without a trigger store, so only
  // the Automations panel shows the unavailable state.
  const automationsPanel = unavailable ? (
    <div className="flex flex-col gap-3">
      <TriggerFamiliesExplainer onOpenSchedules={() => selectTab("schedules")} />
      <div className="py-6">
        <EmptyState
          title="Automations not enabled"
          message={
            cloud
              ? "Event triggers aren't enabled on this server. Cloud repos are automated via forge webhooks and the Schedules tab."
              : "This server has no trigger store wired. Launch a studio with the native tracker to manage event-driven triggers."
          }
          action={
            cloud ? (
              <Button variant="primary" size="sm" onClick={() => selectTab("schedules")}>
                Open Schedules
              </Button>
            ) : undefined
          }
        />
      </div>
    </div>
  ) : (
    <div className="flex flex-col gap-3">
      <p className="text-xs text-fg-muted">
        Event-driven triggers fire a bot when something happens — a board card moves, a run finishes,
        a cron tick, a forge event, or a custom integration. Managed by repo and by bot on one spine.
      </p>

      <TriggerFamiliesExplainer
        onOpenSchedules={() => selectTab("schedules")}
        onNewTrigger={() => setCreatingTrigger(true)}
      />

      {loadErr && (
        <InlineBanner tone="danger" title="Couldn't load triggers">
          {loadErr}
        </InlineBanner>
      )}

      {subs === null && !loadErr ? (
        <div className="flex items-center gap-2 p-6 text-sm text-fg-muted">
          <Spinner /> Loading triggers…
        </div>
      ) : rows.length === 0 ? (
        <EmptyState
          title="No triggers yet"
          message="Create one to launch a bot automatically — e.g. when a card enters “ready” with the “feature” label, or after another bot finishes."
          action={
            <Button variant="primary" size="sm" onClick={() => setCreatingTrigger(true)}>
              New trigger
            </Button>
          }
        />
      ) : (
        <TriggerList subs={rows} onChanged={() => void reload()} />
      )}
    </div>
  );

  return (
    <div className="flex flex-col gap-3 p-4">
      <Tabs
        value={tab}
        onValueChange={(v) => selectTab(v as Tab)}
        items={TABS.map((t) => ({ value: t.id, label: t.label }))}
      />

      {tab === "automations" ? (
        automationsPanel
      ) : (
        <SchedulesTab
          repoFilterParam={repoParam}
          creating={creatingSchedule}
          onCreatingChange={setCreatingSchedule}
          onUnavailable={onSchedUnavailable}
        />
      )}

      <NewTriggerDialog
        open={creatingTrigger}
        onOpenChange={setCreatingTrigger}
        onCreated={() => {
          setCreatingTrigger(false);
          void reload();
        }}
      />
    </div>
  );
}
