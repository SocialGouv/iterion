import { Suspense, lazy, useEffect, useMemo } from "react";

import { ErrorBoundary } from "@/components/shared/ErrorBoundary";
import MainSpinner from "@/components/shared/MainSpinner";
import {
  getOrCreateRunStore,
  RunStoreProvider,
  useRunStore,
} from "@/store/run";
import { useTabsStore } from "@/store/tabs";

const RunView = lazy(() => import("@/components/Runs/RunView"));

interface Props {
  runId: string;
  // The tab id this host renders inside. When provided, the host
  // mirrors the run's friendly name (snapshot.run.name) into the
  // tab label so the tab bar shows something readable rather than
  // a runId prefix.
  tabId?: string;
}

// RunTabHost is the per-tab wrapper for a run subtree. It looks up
// (or creates) the dedicated Zustand store for this runId in the
// registry, hands it down via RunStoreProvider, then renders RunView.
//
// Disposal of the registry entry is driven by useTabsStore.closeTab
// (which calls disposeRunStore explicitly) — NOT by this component's
// unmount. StrictMode would otherwise dispose-then-recreate on every
// mount, dropping freshly-arrived events.
export default function RunTabHost({ runId, tabId }: Props) {
  const store = useMemo(() => getOrCreateRunStore(runId), [runId]);

  return (
    <RunStoreProvider store={store}>
      {tabId && <TabLabelSync tabId={tabId} />}
      <ErrorBoundary area="Run view" resetKey={runId}>
        <Suspense fallback={<MainSpinner />}>
          <RunView runId={runId} />
        </Suspense>
      </ErrorBoundary>
    </RunStoreProvider>
  );
}

// TabLabelSync watches the run snapshot for a friendly name and copies
// it onto the tab. The label survives close/reopen via the tabs store
// localStorage persistence, so the user sees the friendly name
// immediately on cold start without waiting for the snapshot fetch.
// Hosted under RunStoreProvider so the selector hits the per-runId
// store, not the module default.
function TabLabelSync({ tabId }: { tabId: string }) {
  const name = useRunStore((s) => s.snapshot?.run.name ?? null);
  useEffect(() => {
    if (!name) return;
    const tabs = useTabsStore.getState().tabs;
    const current = tabs.find((t) => t.id === tabId);
    if (!current || current.label === name) return;
    useTabsStore.getState().rename(tabId, name);
  }, [name, tabId]);
  return null;
}
