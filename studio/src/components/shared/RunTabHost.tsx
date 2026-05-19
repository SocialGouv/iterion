import { Suspense, lazy, useMemo } from "react";

import { ErrorBoundary } from "@/components/shared/ErrorBoundary";
import MainSpinner from "@/components/shared/MainSpinner";
import {
  getOrCreateRunStore,
  RunStoreProvider,
} from "@/store/run";

const RunView = lazy(() => import("@/components/Runs/RunView"));

interface Props {
  runId: string;
}

// RunTabHost is the per-tab wrapper for a run subtree. It looks up
// (or creates) the dedicated Zustand store for this runId in the
// registry, hands it down via RunStoreProvider, then renders RunView.
//
// Disposal of the registry entry is driven by useTabsStore.closeTab
// (which calls disposeRunStore explicitly) — NOT by this component's
// unmount. StrictMode would otherwise dispose-then-recreate on every
// mount, dropping freshly-arrived events.
export default function RunTabHost({ runId }: Props) {
  const store = useMemo(() => getOrCreateRunStore(runId), [runId]);

  return (
    <RunStoreProvider store={store}>
      <ErrorBoundary area="Run view" resetKey={runId}>
        <Suspense fallback={<MainSpinner />}>
          <RunView runId={runId} />
        </Suspense>
      </ErrorBoundary>
    </RunStoreProvider>
  );
}
