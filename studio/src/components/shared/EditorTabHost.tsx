import { Suspense, lazy, useEffect, useMemo } from "react";

import { ErrorBoundary } from "@/components/shared/ErrorBoundary";
import MainSpinner from "@/components/shared/MainSpinner";
import {
  DocumentStoreProvider,
  getOrCreateDocumentStore,
  useDocumentStore,
} from "@/store/document";
import {
  SelectionStoreProvider,
  getOrCreateSelectionStore,
} from "@/store/selection";
import * as api from "@/api/client";
import { useTabsStore } from "@/store/tabs";
import { useUIStore } from "@/store/ui";

const EditorView = lazy(() => import("@/components/EditorView"));

interface Props {
  tabId: string;
  // When provided, the host opens this file into its document store
  // on first mount. Subsequent renders are no-op (the store keeps the
  // previously-opened document and lets the user edit / save).
  file?: string;
}

// EditorTabHost owns one editor subtree's local state: it instantiates
// (or fetches from registry) the tab's DocumentStore + SelectionStore,
// plumbs them through Context so every component below reads its own
// per-tab data, and triggers the initial `api.openFile` hydration when
// a file path is provided.
//
// Disposal of the per-tab stores is driven by useTabsStore.closeTab,
// not by this component's unmount — StrictMode would otherwise dispose-
// then-recreate fresh, dropping the document on every mount.
export default function EditorTabHost({ tabId, file }: Props) {
  const docStore = useMemo(() => getOrCreateDocumentStore(tabId), [tabId]);
  const selStore = useMemo(() => getOrCreateSelectionStore(tabId), [tabId]);
  const addToast = useUIStore((s) => s.addToast);

  // Initial file hydration. We trigger it once per (tabId, file). If
  // the file changes via deep-link navigation later we let EditorView's
  // existing `?file=` effect handle it — that path is already wired
  // through the per-tab store via Context.
  useEffect(() => {
    if (!file) return;
    const state = docStore.getState();
    if (state.currentFilePath === file) return;
    let cancelled = false;
    void api
      .openFile(file)
      .then((result) => {
        if (cancelled) return;
        const s = docStore.getState();
        if (s.currentFilePath === file) return;
        s.setDocument(result.document);
        s.setCurrentFilePath(result.path);
        s.setCurrentSource(result.source);
        s.setDiagnostics(result.diagnostics);
        s.markSaved();
      })
      .catch((err) => {
        if (cancelled) return;
        addToast(`Open file failed: ${(err as Error).message}`, "error");
      });
    return () => {
      cancelled = true;
    };
  }, [file, docStore, addToast]);

  return (
    <DocumentStoreProvider store={docStore}>
      <SelectionStoreProvider store={selStore}>
        <TabLabelSync tabId={tabId} />
        <ErrorBoundary area="Editor view" resetKey={tabId}>
          <Suspense fallback={<MainSpinner />}>
            <EditorView />
          </Suspense>
        </ErrorBoundary>
      </SelectionStoreProvider>
    </DocumentStoreProvider>
  );
}

function basename(path: string): string {
  const parts = path.split(/[\\/]/);
  return parts[parts.length - 1] || path;
}

// TabLabelSync mirrors the document's current file path onto the tab
// label so opening a file (deep link, RecentFiles click, Save As)
// retitles the tab. Hosted under DocumentStoreProvider so the selector
// hits the per-tab store, not the module default.
function TabLabelSync({ tabId }: { tabId: string }) {
  const path = useDocumentStore((s) => s.currentFilePath);
  useEffect(() => {
    const next = path ? basename(path) : "untitled.bot";
    const tabs = useTabsStore.getState().tabs;
    const current = tabs.find((t) => t.id === tabId);
    if (!current || current.label === next) return;
    useTabsStore.getState().rename(tabId, next);
  }, [path, tabId]);
  return null;
}
