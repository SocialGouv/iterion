import { Pencil2Icon } from "@radix-ui/react-icons";
import { useCallback, useEffect, useMemo } from "react";
import { useLocation, useSearch } from "wouter";
import { useShallow } from "zustand/react/shallow";

import EditorTabHost from "@/components/shared/EditorTabHost";
import InnerTabBar from "@/components/shared/InnerTabBar";
import {
  selectEditorTabs,
  useTabsStore,
} from "@/store/tabs";
import { useUIStore } from "@/store/ui";

// EditorTabsView is the /editor route. It hosts an inner tab strip
// listing every open editor tab (one per .iter file) and renders all
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
  const setCommandPaletteOpen = useUIStore((s) => s.setCommandPaletteOpen);

  const fileParam = useMemo(() => {
    const sp = new URLSearchParams(search);
    return sp.get("file") ?? "";
  }, [search]);

  // URL → tab: when `?file=X` is present, ensure the matching editor
  // tab exists and is active. When the route is `/editor` with no
  // file param and no editor tabs are open yet, auto-create a blank
  // "untitled" tab so the user lands on an empty canvas instead of
  // an empty state. Both branches are idempotent — openTab focuses
  // any existing tab with the same params.
  useEffect(() => {
    const store = useTabsStore.getState();
    if (fileParam) {
      store.openTab("editor", { file: fileParam });
      return;
    }
    const hasAny = store.tabs.some((t) => t.kind === "editor");
    if (!hasAny) {
      store.openTab("editor", {}, "untitled.bot");
    }
  }, [fileParam]);

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

  if (tabs.length === 0) {
    return (
      <div className="h-full flex flex-col">
        <InnerTabBar
          tabs={[]}
          activeTabId={null}
          onSelect={() => {}}
          onClose={() => {}}
          onNewTab={handleNewTab}
          newTabLabel="New editor tab"
          icon={() => <Pencil2Icon className="w-3.5 h-3.5 shrink-0" />}
          emptyState={
            <span>
              No editor tabs open — pick a workflow from{" "}
              <button
                type="button"
                className="underline hover:text-fg-default"
                onClick={() => setCommandPaletteOpen(true)}
              >
                the command palette
              </button>{" "}
              or{" "}
              <button
                type="button"
                className="underline hover:text-fg-default"
                onClick={() => setLocation("/")}
              >
                Home
              </button>
              .
            </span>
          }
        />
        <div className="flex-1 grid place-items-center text-fg-muted text-sm">
          Open a .iter file to start editing.
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
        onNewTab={() => setCommandPaletteOpen(true)}
        newTabLabel="Open a workflow"
        icon={() => <Pencil2Icon className="w-3.5 h-3.5 shrink-0" />}
      />
      <div className="flex-1 min-h-0 relative">
        {tabs.filter((t) => t.hydrated).map((tab) => (
          <div
            key={tab.id}
            className={`absolute inset-0 ${tab.id === activeTabId ? "block" : "hidden"}`}
            aria-hidden={tab.id === activeTabId ? undefined : true}
          >
            <EditorTabHost tabId={tab.id} file={tab.params.file} />
          </div>
        ))}
      </div>
    </div>
  );
}
