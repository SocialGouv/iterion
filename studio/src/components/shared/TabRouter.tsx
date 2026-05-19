import type { ReactNode } from "react";

import RunTabHost from "./RunTabHost";
import { useTabsStore } from "@/store/tabs";

// TabRouter renders one tab subtree at a time for non-run kinds, and
// keeps every hydrated run tab mounted in parallel (hidden via
// display: none for the non-active ones). The wouter <Switch> upstream
// remains the renderer for non-tab routes (/runs/new, etc.) — see
// AppShell. Background-mounted run subtrees keep their WebSocket
// connections alive so live events accumulate even when the user is
// looking at a different tab.
//
// Non-run kinds (home, editor, runs, whats-next, board, dispatcher,
// settings, team) are still served by wouter's <Switch> in Phase 2 —
// the URL is kept in sync with the active tab, so each route mounts
// the matching view. This will change in Phase 3 when EditorTabHost
// lands and editor tabs gain isolated state too.
export default function TabRouter({ fallback }: { fallback: ReactNode }) {
  const tabs = useTabsStore((s) => s.tabs);
  const activeTabId = useTabsStore((s) => s.activeTabId);
  const runTabs = tabs.filter(
    (t) => t.kind === "run" && t.hydrated && t.params.runId,
  );
  const activeIsRun = tabs.find((t) => t.id === activeTabId)?.kind === "run";

  return (
    <>
      {/* Run tabs: every hydrated one stays mounted; only the active
          one is visible. Hidden tabs keep their WS connection so
          events accumulate while the user is looking elsewhere. */}
      {runTabs.map((tab) => (
        <div
          key={tab.id}
          className={`h-full w-full ${tab.id === activeTabId ? "block" : "hidden"}`}
          aria-hidden={tab.id === activeTabId ? undefined : true}
        >
          <RunTabHost runId={tab.params.runId!} />
        </div>
      ))}
      {/* Non-run tabs: defer to the wouter-managed fallback (Switch).
          We hide the fallback when a run tab is active so the live
          run view is the only thing visible. */}
      <div className={`h-full w-full ${activeIsRun ? "hidden" : "block"}`}>
        {fallback}
      </div>
    </>
  );
}

