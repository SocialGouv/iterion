import { Pencil2Icon } from "@radix-ui/react-icons";
import { useCallback, useEffect, useMemo } from "react";
import { useLocation, useSearch } from "wouter";
import { useShallow } from "zustand/react/shallow";

import EditorTabHost from "@/components/shared/EditorTabHost";
import InnerTabBar from "@/components/shared/InnerTabBar";
import RecentFilesPanel from "@/components/Home/RecentFilesPanel";
import {
  selectEditorTabs,
  useTabsStore,
} from "@/store/tabs";

// EditorTabsView is the /editor route. It hosts an inner tab strip
// listing every open editor tab (one per .bot file) and renders all
// hydrated tabs in parallel with display:none on inactive ones so
// switching is instant and dirty state survives.
//
// URL ↔ tab sync is one-directional via effect (URL → tab on deep-link
// or sidebar navigation) and synchronous via callbacks (tab → URL on
// user click). The bidirectional-effect pattern is intentionally
// avoided here — every effect-driven URL push risks an effect re-fire
// loop because wouter's setLocation reference and the persist
// middleware's hydration can each invalidate the deps array.
export default function EditorTabsView() {
  const search = useSearch();
  const [, setLocation] = useLocation();
  // useShallow lets Zustand compare the filtered array element-by-element
  // so unrelated tab mutations don't fail the same-reference check and
  // re-render this view (the filter would otherwise produce a fresh
  // array identity on every store update → React aborts with
  // "Maximum update depth exceeded" via the getSnapshot caching rule).
  const tabs = useTabsStore(useShallow(selectEditorTabs));
  const activeTabId = useTabsStore((s) => s.activeEditorTabId);

  // Assign, don't merely open: see the invariant note below.
  const ensureActive = (id: string) => {
    if (useTabsStore.getState().activeEditorTabId !== id) {
      useTabsStore.getState().setActive(id);
    }
  };

  const fileParam = useMemo(() => {
    const sp = new URLSearchParams(search);
    return sp.get("file") ?? "";
  }, [search]);

  // `?draft=<runId>` — the assistant drafted a `.bot` and proposed opening it
  // here. A draft has no path (the bot never wrote to the workspace), so it
  // keys its own tab and hydrates into an unsaved buffer.
  const draftParam = useMemo(() => {
    const sp = new URLSearchParams(search);
    return sp.get("draft") ?? "";
  }, [search]);

  // URL → tab: when `?file=X` is present, ensure the matching editor
  // tab exists and is active. Idempotent on repeat because openTab
  // focuses an existing tab with the same params. When the URL is
  // bare `/editor` and no tabs exist, the empty-state view (below)
  // takes over and presents the picker instead of an empty editor.
  useEffect(() => {
    if (!fileParam) return;
    ensureActive(useTabsStore.getState().openTab("editor", { file: fileParam }));
  }, [fileParam, tabs, activeTabId]);

  // Same contract as `?file=`: idempotent, because openTab focuses an
  // existing tab carrying identical params rather than opening a second one.
  useEffect(() => {
    if (!draftParam || fileParam) return;
    ensureActive(
      useTabsStore.getState().openTab("editor", { draft: draftParam }, "Draft"),
    );
  }, [draftParam, fileParam, tabs, activeTabId]);

  // The invariant both effects above keep: if the URL names a document, ITS
  // tab is the one on screen.
  //
  // openTab already activates what it opens — but the persist middleware
  // rehydrates `activeEditorTabId` from localStorage and can land AFTER these
  // effects, restoring a previously-active tab over the one the URL just
  // asked for. The operator then reads a stale tab's state as the answer to
  // the link they clicked (an old draft's "couldn't reload", say) and
  // concludes the link is broken.
  //
  // Re-asserting on every tabs/active change makes the effects self-correcting
  // whatever the ordering. It cannot fight a manual tab click either: selecting
  // a tab rewrites the URL first (to `?file=…` or bare `/editor`), so the guard
  // above returns before reaching here.

  // Cold-start restore: the persisted ACTIVE tab rehydrates dormant
  // (hydrated:false) and is never clicked, so without this it shows selected in
  // the strip while the pane below stays blank (only hydrated tabs mount).
  // Re-activating it hydrates it + mounts its EditorTabHost. A null/dangling
  // active id is deliberately NOT repointed to tabs[0]: null active = the editor
  // home (welcome pane), which the "Editor" nav link returns to on demand.
  useEffect(() => {
    if (!activeTabId) return;
    const active = tabs.find((t) => t.id === activeTabId);
    if (active && !active.hydrated) {
      useTabsStore.getState().setActive(active.id);
    }
  }, [tabs, activeTabId]);

  // Tab → URL: synchronous on user action. Click → activate + push URL.
  const handleSelect = useCallback(
    (id: string) => {
      useTabsStore.getState().setActive(id);
      const tab = useTabsStore.getState().tabs.find((t) => t.id === id);
      const file = tab?.params.file ?? "";
      const target = file ? `/editor?file=${encodeURIComponent(file)}` : "/editor";
      setLocation(target, { replace: true });
    },
    [setLocation],
  );

  // "+" button → create a fresh untitled tab (always new, never
  // refocuses an existing untitled tab). Drops the file query param
  // so the new tab isn't immediately hydrated from a stale URL.
  const handleNewTab = useCallback(() => {
    useTabsStore.getState().newEditorTab();
    setLocation("/editor", { replace: true });
  }, [setLocation]);

  // Close: dispose the tab + sync URL to the new active tab (or
  // /editor if none remain).
  const handleClose = useCallback(
    (id: string) => {
      useTabsStore.getState().closeTab(id);
      const next = useTabsStore.getState();
      const newActive = next.tabs.find((t) => t.id === next.activeEditorTabId);
      const file = newActive?.params.file ?? "";
      const target = file ? `/editor?file=${encodeURIComponent(file)}` : "/editor";
      setLocation(target, { replace: true });
    },
    [setLocation],
  );

  // The editor "home" (welcome + recent files) shows whenever no tab is active
  // — no tabs open at all, OR the user returned to it via the "Editor" nav link
  // (which deselects the active tab). The tab strip stays visible so open tabs
  // remain one click away.
  const activeTab = tabs.find((t) => t.id === activeTabId);
  const showHome = !activeTab;

  return (
    <div className="h-full flex flex-col">
      <InnerTabBar
        tabs={tabs}
        activeTabId={showHome ? null : activeTabId}
        onSelect={handleSelect}
        onClose={handleClose}
        onNewTab={handleNewTab}
        newTabLabel="New editor tab"
        icon={() => <Pencil2Icon className="w-3.5 h-3.5 shrink-0" />}
      />
      {showHome ? (
        <div className="flex-1 overflow-auto">
          <div className="max-w-md mx-auto py-10 px-4 space-y-4">
            <div>
              <h2 className="text-sm font-semibold text-fg-default">
                The same workflows, visually.
              </h2>
              <p className="text-xs text-fg-muted mt-1">
                Build on the canvas or edit the <code className="font-mono">.bot</code> source
                directly — both stay in sync. Pick an existing file, fork an example, or start
                with a blank canvas; each opens in its own tab.
              </p>
            </div>
            <RecentFilesPanel variant="plain" />
          </div>
        </div>
      ) : (
        <div className="flex-1 min-h-0 relative">
          {tabs
            .filter((t) => t.hydrated)
            .map((tab) => (
              <div
                key={tab.id}
                className={`absolute inset-0 ${tab.id === activeTabId ? "block" : "hidden"}`}
                aria-hidden={tab.id === activeTabId ? undefined : true}
              >
                <EditorTabHost
                  tabId={tab.id}
                  file={tab.params.file}
                  draft={tab.params.draft}
                />
              </div>
            ))}
        </div>
      )}
    </div>
  );
}
