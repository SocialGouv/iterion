import { ListBulletIcon, PlayIcon } from "@radix-ui/react-icons";
import { useCallback, useEffect } from "react";
import { useLocation, useParams } from "wouter";
import { useShallow } from "zustand/react/shallow";

import RunTabHost from "@/components/shared/RunTabHost";
import InnerTabBar from "@/components/shared/InnerTabBar";
import {
  selectRunTabs,
  useTabsStore,
} from "@/store/tabs";

// RunsTabsView is the /runs/:id route. Same architecture as
// EditorTabsView: URL → tab on deep-link (effect), tab → URL on user
// click (callback). See the comment there for the no-bidirectional-
// effect rationale.
export default function RunsTabsView() {
  const params = useParams<{ id: string }>();
  const [, setLocation] = useLocation();
  // See EditorTabsView for the rationale on useShallow.
  const tabs = useTabsStore(useShallow(selectRunTabs));
  const activeTabId = useTabsStore((s) => s.activeRunTabId);

  // URL → tab.
  useEffect(() => {
    if (!params.id) return;
    useTabsStore.getState().openTab("run", { runId: params.id });
  }, [params.id]);

  const handleSelect = useCallback(
    (id: string) => {
      useTabsStore.getState().setActive(id);
      const tab = useTabsStore.getState().tabs.find((t) => t.id === id);
      const runId = tab?.params.runId;
      if (!runId) return;
      setLocation(`/runs/${encodeURIComponent(runId)}`, { replace: true });
    },
    [setLocation],
  );

  const handleClose = useCallback(
    (id: string) => {
      useTabsStore.getState().closeTab(id);
      const next = useTabsStore.getState();
      const newActive = next.tabs.find((t) => t.id === next.activeRunTabId);
      const newRunId = newActive?.params.runId;
      setLocation(newRunId ? `/runs/${encodeURIComponent(newRunId)}` : "/runs", {
        replace: true,
      });
    },
    [setLocation],
  );

  const pinnedRunsList = {
    icon: <ListBulletIcon className="w-3.5 h-3.5 shrink-0" />,
    label: "All runs",
    onClick: () => setLocation("/runs"),
  };

  if (tabs.length === 0) {
    return (
      <div className="h-full flex flex-col">
        <InnerTabBar
          tabs={[]}
          activeTabId={null}
          onSelect={() => {}}
          onClose={() => {}}
          pinnedLead={pinnedRunsList}
          icon={() => <PlayIcon className="w-3.5 h-3.5 shrink-0" />}
          emptyState={
            <span>
              No run open — pick one from{" "}
              <button
                type="button"
                className="underline hover:text-fg-default"
                onClick={() => setLocation("/runs")}
              >
                the runs list
              </button>
              .
            </span>
          }
        />
        <div className="flex-1 grid place-items-center text-fg-muted text-sm">
          Open a run to view its progress.
        </div>
      </div>
    );
  }

  return (
    <div className="h-full flex flex-col">
      <InnerTabBar
        tabs={tabs}
        activeTabId={activeTabId}
        onSelect={handleSelect}
        onClose={handleClose}
        pinnedLead={pinnedRunsList}
        icon={() => <PlayIcon className="w-3.5 h-3.5 shrink-0" />}
      />
      <div className="flex-1 min-h-0 relative">
        {tabs.filter((t) => t.hydrated).map((tab) => {
          const runId = tab.params.runId;
          if (!runId) return null;
          return (
            <div
              key={tab.id}
              className={`absolute inset-0 ${tab.id === activeTabId ? "block" : "hidden"}`}
              aria-hidden={tab.id === activeTabId ? undefined : true}
            >
              <RunTabHost runId={runId} tabId={tab.id} />
            </div>
          );
        })}
      </div>
    </div>
  );
}
