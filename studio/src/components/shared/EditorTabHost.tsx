import { Suspense, lazy, useCallback, useEffect, useMemo, useState } from "react";
import { ExclamationTriangleIcon } from "@radix-ui/react-icons";
import { useLocation } from "wouter";

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
import { parseSource } from "@/api/client";
import { findDraftBotSource } from "@/api/runs/artifacts";
import { isDefaultTabLabel, useTabsStore } from "@/store/tabs";
import { useBotsStore } from "@/store/bots";
import { useUIStore } from "@/store/ui";
import { botDisplayLabel } from "@/lib/botLabel";
import { toastError } from "@/lib/errorHints";
import { Button, EmptyState } from "@/components/ui";

const EditorView = lazy(() => import("@/components/EditorView"));

type LoadState = "ready" | "loading" | "error";

interface Props {
  tabId: string;
  // When provided, the host opens this file into its document store
  // on first mount. Subsequent renders are no-op (the store keeps the
  // previously-opened document and lets the user edit / save).
  file?: string;
  // Run id of a conversation that drafted a `.bot`. The tab is seeded with
  // that draft as an UNSAVED buffer — no file path, so the first save is a
  // Save As and the operator chooses where it lands. This is the whole of the
  // assistant's write authority here: it produced text, the operator carries
  // it to disk. Ignored once the tab has a source (never clobbers edits).
  draft?: string;
}

// EditorTabHost owns one editor subtree's local state: it instantiates
// (or fetches from registry) the tab's DocumentStore + SelectionStore,
// plumbs them through Context so every component below reads its own
// per-tab data, and triggers the initial `api.openFile` hydration when
// a file path is provided. While that hydration is in flight it shows a
// spinner — never the untitled scaffold the store initializes with —
// and on failure an explicit "couldn't reload" state.
//
// Disposal of the per-tab stores is driven by useTabsStore.closeTab,
// not by this component's unmount — StrictMode would otherwise dispose-
// then-recreate fresh, dropping the document on every mount.
export default function EditorTabHost({ tabId, file, draft }: Props) {
  const docStore = useMemo(() => getOrCreateDocumentStore(tabId), [tabId]);
  const selStore = useMemo(() => getOrCreateSelectionStore(tabId), [tabId]);
  const addToast = useUIStore((s) => s.addToast);
  // Visibility of this tab. EditorTabsView keeps every hydrated tab
  // mounted with display:none on inactive ones; React Flow mounted in a
  // hidden container can't measure and ends up blank when re-shown, so
  // the Canvas needs to know when it regains visibility to refit the
  // viewport. Drives that signal down through EditorView.
  const isActive = useTabsStore((s) => s.activeEditorTabId === tabId);
  const tab = useTabsStore((s) => s.tabs.find((t) => t.id === tabId));

  const [loadState, setLoadState] = useState<LoadState>(() => {
    if (file) return docStore.getState().currentFilePath !== file ? "loading" : "ready";
    // A fresh store has a null source; anything else means the tab already
    // carries a document we must not replace.
    if (draft) return docStore.getState().currentSource === null ? "loading" : "ready";
    return "ready";
  });
  const [loadError, setLoadError] = useState<string | null>(null);
  const [retryNonce, setRetryNonce] = useState(0);

  // A toast is app-level, so it reads as the answer to whatever the operator
  // just did. But EVERY hydrated tab is mounted (inactive ones are only
  // display:none), so a BACKGROUND tab failing to reload — a draft whose run
  // is gone, a file that moved — would raise one over an unrelated screen.
  // That is how a working link came to look broken.
  //
  // The failure is not swallowed: the tab renders TabLoadErrorState inline,
  // with Retry and Close, which is where it belongs. Toast only for the tab
  // the operator is actually looking at, read at failure time rather than
  // captured when the effect ran.
  const toastIfOnScreen = useCallback(
    (err: unknown, title: string) => {
      if (useTabsStore.getState().activeEditorTabId !== tabId) return;
      toastError(addToast, err, title);
    },
    [tabId, addToast],
  );

  // Initial file hydration. We trigger it once per (tabId, file). If
  // the file changes via deep-link navigation later we let EditorView's
  // existing `?file=` effect handle it — that path is already wired
  // through the per-tab store via Context.
  useEffect(() => {
    if (!file) return;
    if (docStore.getState().currentFilePath === file) {
      setLoadState("ready");
      return;
    }
    let cancelled = false;
    setLoadState("loading");
    setLoadError(null);
    void api
      .openFile(file)
      .then((result) => {
        if (cancelled) return;
        const s = docStore.getState();
        // Another path (deep link, Save As) may have bound the file
        // while the fetch was in flight — don't clobber it.
        if (s.currentFilePath !== file) {
          s.setDocument(result.document);
          s.setCurrentFilePath(result.path);
          s.setCurrentSource(result.source);
          s.setDiagnostics(result.diagnostics);
          s.markSaved();
        }
        setLoadState("ready");
      })
      .catch((err) => {
        if (cancelled) return;
        setLoadState("error");
        setLoadError(err instanceof Error ? err.message : String(err));
        toastIfOnScreen(err, "Open file failed");
      });
    return () => {
      cancelled = true;
    };
  }, [file, docStore, toastIfOnScreen, retryNonce]);

  // Draft hydration — the assistant's counterpart to opening a file. The
  // draft lives in the conversation's artifact, never on disk, so it is
  // fetched and parsed into the same shape openFile produces. Deliberately
  // NOT markSaved(): the buffer is dirty from the first frame, because
  // nothing has written it anywhere yet.
  useEffect(() => {
    if (!draft || file) return;
    if (docStore.getState().currentSource !== null) {
      setLoadState("ready");
      return;
    }
    let cancelled = false;
    const ctrl = new AbortController();
    setLoadState("loading");
    setLoadError(null);
    void findDraftBotSource(draft, { signal: ctrl.signal })
      .then(async (source) => {
        if (cancelled) return;
        if (!source) {
          throw new Error(
            "That conversation has no .bot draft to open — ask the assistant to draft one first.",
          );
        }
        const parsed = await parseSource(source);
        if (cancelled) return;
        const st = docStore.getState();
        // Same race guard as the file path: the operator may have started
        // typing while the fetch was in flight.
        if (st.currentSource === null) {
          st.setDocument(parsed.document);
          st.setCurrentSource(source);
          st.setDiagnostics(parsed.diagnostics);
        }
        setLoadState("ready");
      })
      .catch((err) => {
        if (cancelled) return;
        setLoadState("error");
        setLoadError(err instanceof Error ? err.message : String(err));
        toastIfOnScreen(err, "Open draft failed");
      });
    return () => {
      cancelled = true;
      ctrl.abort();
    };
  }, [draft, file, docStore, toastIfOnScreen, retryNonce]);

  // A tab restored from localStorage whose label names a file but whose
  // params carry none can't reload its document — surface that instead
  // of the scaffold (data-loss hazard: the user would edit a fresh
  // scaffold believing it's their bot). A default label (isDefaultTabLabel)
  // marks a legitimate untitled scaffold; in-session tabs mid-open —
  // example fork, toolbar Open — legitimately have no file param yet and
  // are excluded by `restored`.
  // A DRAFT tab has no file by construction and is re-hydratable from its
  // run's artifact, so it is not a lost binding — excluded explicitly or a
  // reload turns every draft into "couldn't reload".
  const lostBinding =
    !file &&
    !draft &&
    !!tab?.restored &&
    !!tab.label &&
    !isDefaultTabLabel(tab.label);

  let body;
  if (lostBinding) {
    body = (
      <TabLoadErrorState
        tabId={tabId}
        title={`Couldn't reload “${tab!.label}”`}
        message="This tab lost its link to the file it was editing. Reopen the file from Home → Recent files or the file picker."
      />
    );
  } else if ((file || draft) && loadState === "loading") {
    body = <MainSpinner />;
  } else if ((file || draft) && loadState === "error") {
    body = (
      <TabLoadErrorState
        tabId={tabId}
        title={`Couldn't reload “${tab?.label ?? file}”`}
        message={loadError ?? "The file could not be opened."}
        onRetry={() => setRetryNonce((n) => n + 1)}
      />
    );
  } else {
    body = <EditorView active={isActive} />;
  }

  return (
    <DocumentStoreProvider store={docStore}>
      <SelectionStoreProvider store={selStore}>
        <TabBindingSync tabId={tabId} />
        <ErrorBoundary area="Editor view" resetKey={tabId}>
          <Suspense fallback={<MainSpinner />}>{body}</Suspense>
        </ErrorBoundary>
      </SelectionStoreProvider>
    </DocumentStoreProvider>
  );
}

// Explicit non-scaffold state for a tab whose document can't be shown:
// restored without a file binding, or the file failed to load. Keeps the
// tab (and its name) visible so the user understands what's missing, and
// never hands them an editable untitled scaffold under that name.
function TabLoadErrorState({
  tabId,
  title,
  message,
  onRetry,
}: {
  tabId: string;
  title: string;
  message: string;
  onRetry?: () => void;
}) {
  const [, setLocation] = useLocation();
  const closeButton = (
    <Button
      variant="secondary"
      size="sm"
      onClick={() => {
        useTabsStore.getState().closeTab(tabId);
        const next = useTabsStore.getState();
        const newActive = next.tabs.find(
          (t) => t.id === next.activeEditorTabId,
        );
        const f = newActive?.params.file ?? "";
        setLocation(f ? `/editor?file=${encodeURIComponent(f)}` : "/editor", {
          replace: true,
        });
      }}
    >
      Close tab
    </Button>
  );
  return (
    <EmptyState
      className="bg-surface-0"
      icon={<ExclamationTriangleIcon className="h-6 w-6 text-warning" />}
      title={title}
      message={message}
      action={
        onRetry ? (
          <Button variant="primary" size="sm" onClick={onRetry}>
            Retry
          </Button>
        ) : (
          closeButton
        )
      }
      secondaryAction={onRetry ? closeButton : undefined}
    />
  );
}

// TabBindingSync mirrors the document's current file path onto the tab —
// both the label AND the params.file binding — so opening a file through
// any path (deep link, RecentFiles click, toolbar Open, Save As, example
// fork) retitles the tab and keeps it reloadable after a page reload.
// Hosted under DocumentStoreProvider so the selector hits the per-tab
// store, not the module default.
//
// Uses botDisplayLabel so a bundle's `main.bot` shows the persona
// display_name (e.g. "Featurly") / technical id ("feature-dev") rather
// than the non-distinctive basename "main.bot". Only acts when
// `currentFilePath` is non-null. Resetting label/params whenever path is
// null would race the openFile resolution on every new tab open and
// clobber values set by the caller.
function TabBindingSync({ tabId }: { tabId: string }) {
  const path = useDocumentStore((s) => s.currentFilePath);
  const bots = useBotsStore((s) => s.bots);
  const fetchBots = useBotsStore((s) => s.fetch);
  useEffect(() => {
    // A bot bundle's main.bot needs the catalog to resolve its persona
    // name; fetch it lazily so the tab can settle on "Featurly".
    if (path && bots === null) void fetchBots();
  }, [path, bots, fetchBots]);
  useEffect(() => {
    if (!path) return;
    useTabsStore.getState().bindFile(tabId, path);
    const next = botDisplayLabel(path, bots);
    const tabs = useTabsStore.getState().tabs;
    const current = tabs.find((t) => t.id === tabId);
    if (!current || current.label === next) return;
    useTabsStore.getState().rename(tabId, next);
  }, [path, bots, tabId]);
  return null;
}
