import {
  Suspense,
  lazy,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { ExclamationTriangleIcon } from "@radix-ui/react-icons";
import { useLocation } from "wouter";
import { useStore } from "zustand";

import { ErrorBoundary } from "@/components/shared/ErrorBoundary";
import MainSpinner from "@/components/shared/MainSpinner";
import {
  DocumentStoreProvider,
  getOrCreateDocumentStore,
  useDocumentStore,
  type DocumentState,
  type SourceBuffer,
} from "@/store/document";
import {
  SelectionStoreProvider,
  getOrCreateSelectionStore,
} from "@/store/selection";
import * as api from "@/api/client";
import { parseSource } from "@/api/client";
import { useQuery } from "@tanstack/react-query";

import { findDraftBotSource } from "@/api/runs/artifacts";
import { editorDraftKey } from "@/hooks/useDraftBot";
import { applyOpenedFile } from "@/lib/openedFile";
import { applyParsedSource } from "@/lib/salvage";
import { isDefaultTabLabel, useTabsStore } from "@/store/tabs";
import { useDropEditorTab } from "@/hooks/useDropEditorTab";
import { useBotsStore } from "@/store/bots";
import { useUIStore } from "@/store/ui";
import { botDisplayLabel } from "@/lib/botLabel";
import { toastError } from "@/lib/errorHints";
import { Button, EmptyState } from "@/components/ui";

const EditorView = lazy(() => import("@/components/EditorView"));

type LoadState = "ready" | "loading" | "error";

// A safety net only — the dock's invalidation is what actually refreshes an
// open draft tab. Generous on purpose: this exists for the case where nothing
// is around to invalidate, not for the normal path.
const DRAFT_FALLBACK_REFETCH_MS = 30000;

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
          // Through the shared helper: a file that does not parse comes with
          // the word that its document is a SALVAGE, which is what refuses
          // the write. Hand-rolled setters here were one of the sites that
          // kept marking such a document saved.
          applyOpenedFile(result, s);
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

  // Draft hydration — the assistant's counterpart to opening a file. The draft
  // lives in the conversation's artifact, never on disk, so it is fetched and
  // parsed into the same shape openFile produces. Deliberately NOT
  // markSaved(): the buffer is dirty from the first frame, because nothing has
  // written it anywhere yet.
  //
  // This FOLLOWS the conversation rather than seeding once: the assistant
  // redrafts across turns ("now add a judge"), and a tab that only took its
  // first draft left the canvas frozen while the assistant announced an update
  // the operator could not see.
  //
  // It does not poll. The dock invalidates this key when a turn lands (see
  // ChatDock), which is the earliest moment there is anything new to read —
  // the draft is a node OUTPUT, written at `artifact_written`, so it does not
  // exist until the turn is over. The slow refetch below is a net for the case
  // where nothing is there to invalidate, not the mechanism.
  const draftQuery = useQuery({
    queryKey: editorDraftKey(draft ?? ""),
    queryFn: ({ signal }) => findDraftBotSource(draft as string, { signal }),
    enabled: !!draft && !file,
    staleTime: Infinity,
    // Stop polling once the lookup has failed — a pruned/cancelled run
    // would otherwise walk the whole artifact list every 30s for the
    // tab's lifetime (R0bb32f).
    refetchInterval: (q) => (q.state.status === "error" ? false : DRAFT_FALLBACK_REFETCH_MS),
    retry: false,
  });

  const appliedRef = useRef<string | null>(null);
  // What the last hydration INSTALLED: the generation it left the document
  // at, and the Source buffer as it was. The canvas is the author's once
  // either moved — a canvas edit moves the generation and not
  // `currentSource`, which is why comparing the source alone let a later
  // draft replace canvas edits.
  const installedRef = useRef<{ generation: number; source: SourceBuffer | null } | null>(null);
  // An explicit replacement the author asked for (an Open still loading)
  // wins: a draft landing first would get its answer refused for edits
  // nobody made. The draft waits, and is looked at again once it ends.
  // Read from `docStore` itself: this host sits ABOVE the provider it
  // renders, so the context hook would answer for another store.
  const replacementPending = useStore(docStore, (s) => s._pendingIntent !== null);
  useEffect(() => {
    if (!draft || file) return;
    const source = draftQuery.data;

    if (draftQuery.isPending) return;
    if (draftQuery.isError) {
      if (appliedRef.current !== null) return; // keep a canvas already in use
      setLoadState("error");
      const err = draftQuery.error;
      setLoadError(err instanceof Error ? err.message : String(err));
      toastIfOnScreen(err, "Open draft failed");
      return;
    }
    if (!source) {
      // Only the FIRST look is an error. A later read that finds nothing is
      // transient, not a reason to blank a canvas already in use.
      if (appliedRef.current !== null) return;
      const missing = new Error(
        "That conversation has no .bot draft to open — ask the assistant to draft one first.",
      );
      setLoadState("error");
      setLoadError(missing.message);
      toastIfOnScreen(missing, "Open draft failed");
      return;
    }
    if (source === appliedRef.current) return;

    // Never clobber the operator. We own the buffer only while it still holds
    // exactly what we last put there; the moment they edit it — the source,
    // the canvas, or the Source view's text — it is theirs, and a new draft
    // waits for them to ask for it.
    const theirs = (st: DocumentState) => {
      if (st.currentSource !== null && st.currentSource !== appliedRef.current) return true;
      const installed = installedRef.current;
      return !!installed && (st._generation !== installed.generation || st.sourceBuffer !== installed.source);
    };
    if (replacementPending || theirs(docStore.getState())) return;

    let cancelled = false;
    void parseSource(source)
      .then((parsed) => {
        if (cancelled) return;
        const st2 = docStore.getState();
        // They started editing while we were parsing, or asked for another
        // document.
        if (st2._pendingIntent !== null || theirs(st2)) return;
        // A replacement: a Save As still writing the previous draft does
        // not bind its name to this one.
        st2.markReplaced();
        // Document and verdict together: a draft whose source does not parse
        // whole is a salvage, and writing it back would drop what the parser
        // could not read.
        applyParsedSource(parsed, st2);
        st2.setCurrentSource(source);
        st2.setDiagnostics(parsed.diagnostics);
        appliedRef.current = source;
        const after = docStore.getState();
        installedRef.current = { generation: after._generation, source: after.sourceBuffer };
        setLoadState("ready");
      })
      .catch((err) => {
        if (cancelled) return;
        if (appliedRef.current !== null) return;
        setLoadState("error");
        setLoadError(err instanceof Error ? err.message : String(err));
        toastIfOnScreen(err, "Open draft failed");
      });
    return () => {
      cancelled = true;
    };
  }, [
    draft,
    file,
    docStore,
    replacementPending,
    toastIfOnScreen,
    draftQuery.data,
    draftQuery.isPending,
    draftQuery.isError,
    draftQuery.error,
  ]);

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
        onRetry={() => {
          setRetryNonce((n) => n + 1);
          if (draft && !file) void draftQuery.refetch();
        }}
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
  // Through the same guard as every other editor-tab close: the tab's store
  // can still hold unsaved work under this card — a draft tab shown again
  // after its draft stopped being readable keeps what the author was
  // editing — and closing disposes it.
  const { guardDroppingEditorTab, dialog } = useDropEditorTab();
  const closeButton = (
    <Button
      variant="secondary"
      size="sm"
      onClick={() =>
        void guardDroppingEditorTab(() => {
          useTabsStore.getState().closeTab(tabId);
          const next = useTabsStore.getState();
          const newActive = next.tabs.find(
            (t) => t.id === next.activeEditorTabId,
          );
          const f = newActive?.params.file ?? "";
          setLocation(f ? `/editor?file=${encodeURIComponent(f)}` : "/editor", {
            replace: true,
          });
        }, tabId)
      }
    >
      Close tab
    </Button>
  );
  return (
    <>
    {dialog}
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
    </>
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
// than the non-distinctive basename "main.bot".
//
// The mirror runs both ways, on two different facts. A path binds. A null
// path unbinds — params and label — only when the store says the document
// was DETACHED (File → New, Import, Start blank): a tab that kept naming its
// previous file or draft would fetch it over the author's work on the next
// mount. The null of a store that has not resolved yet is left alone: a tab
// just opened for a file carries the param its load is for, and dropping it
// then would cancel the load and clobber the caller's label.
//
// The URL is part of the same binding. EditorTabsView re-asserts "the URL's
// document is on screen" on every change of the tabs, and opens a tab for a
// document no tab names — so when the ACTIVE tab stops naming what the URL
// names (a file, or a draft it never saved), the URL is rewritten bare
// first, the way a tab click rewrites it; on a rebind, to the new file. The
// initial load, where the param already names the path, writes nothing: the
// deep link's `?node=` and `?from=` must reach EditorView.
function TabBindingSync({ tabId }: { tabId: string }) {
  const path = useDocumentStore((s) => s.currentFilePath);
  const detached = useDocumentStore((s) => s.detached);
  const bots = useBotsStore((s) => s.bots);
  const fetchBots = useBotsStore((s) => s.fetch);
  const [, setLocation] = useLocation();
  useEffect(() => {
    // A bot bundle's main.bot needs the catalog to resolve its persona
    // name; fetch it lazily so the tab can settle on "Featurly".
    if (path && bots === null) void fetchBots();
  }, [path, bots, fetchBots]);
  useEffect(() => {
    const tabsStore = useTabsStore.getState();
    const current = tabsStore.tabs.find((t) => t.id === tabId);
    if (!current) return;
    const onScreen = tabsStore.activeEditorTabId === tabId;
    if (!path) {
      if (!detached) return;
      if (onScreen && (current.params.file || current.params.draft))
        setLocation("/editor", { replace: true });
      tabsStore.unbindFile(tabId);
      return;
    }
    if (current.params.file !== path) {
      if (onScreen) setLocation(`/editor?file=${encodeURIComponent(path)}`, { replace: true });
      tabsStore.bindFile(tabId, path);
    }
    const next = botDisplayLabel(path, bots);
    if (current.label === next) return;
    tabsStore.rename(tabId, next);
  }, [path, detached, bots, tabId, setLocation]);
  return null;
}
