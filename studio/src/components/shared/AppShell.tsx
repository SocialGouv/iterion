import { Suspense } from "react";
import type { ReactNode } from "react";
import { useLocation } from "wouter";

import Sidebar from "./Sidebar";
import ContextualHeaderBar from "./ContextualHeaderBar";
import MainSpinner from "./MainSpinner";
import { useAssistantReservedWidthPx } from "@/components/ChatDock/AssistantProvider";
import { useUIStore } from "@/store/ui";

interface AppShellProps {
  children: ReactNode;
}

// AppShell is the persistent layout root for all authenticated routes.
// Sidebar + ContextualHeaderBar stay mounted across navigation; only
// <main> swaps its lazy-loaded route content. Routing is driven by
// the wouter <Switch> passed in as children — sections like /editor
// and /runs/:id host their own inner tab strips for multi-file /
// multi-run parallelism (see EditorTabsView / RunsTabsView).
//
// Focus mode: when useUIStore.expanded is true (editor canvas-only
// mode), the chrome is dropped entirely — including the "Skip to
// main content" link, since there's nothing to skip past.
export default function AppShell({ children }: AppShellProps) {
  const expanded = useUIStore((s) => s.expanded);
  const [location] = useLocation();
  // The assistant dock pins its own column at the right edge (it lives
  // outside this layout tree, next to the command palette). Reserve its
  // width — box-sizing is border-box, so the padding eats into the
  // h-screen box — so it pushes the page aside instead of covering it.
  const dockedWidth = useAssistantReservedWidthPx();
  // Key the content wrapper by the top-level view segment (not the full
  // path) so switching views (/runs → /board) plays a gentle opacity
  // fade, while intra-view URL churn (run id, selected node, ?file=)
  // does NOT re-trigger it. Opacity-only — the translateY fade would
  // flash the scrollbar on edge-pinned panels (see app.css).
  const viewKey = location === "/" ? "home" : location.split("/")[1] || "home";

  if (expanded) {
    // Focus mode drops the chrome, but the dock is not chrome — it is
    // mounted outside this tree and stays reachable, so the reservation
    // applies here too.
    return (
      <div
        className="h-screen w-screen bg-surface-0 text-fg-default"
        style={dockedWidth ? { paddingRight: dockedWidth } : undefined}
      >
        <Suspense fallback={<MainSpinner />}>{children}</Suspense>
      </div>
    );
  }

  return (
    <div
      className="h-screen w-screen flex bg-surface-0 text-fg-default overflow-hidden"
      style={dockedWidth ? { paddingRight: dockedWidth } : undefined}
    >
      <a
        href="#main-content"
        className="sr-only focus:not-sr-only focus:fixed focus:top-2 focus:left-2 focus:z-[var(--z-toast)] focus:bg-accent focus:text-fg-onAccent focus:px-3 focus:py-1.5 focus:rounded focus:shadow-[var(--shadow-md)]"
      >
        Skip to main content
      </a>
      <Sidebar />
      <div className="flex-1 min-w-0 flex flex-col">
        <ContextualHeaderBar />
        <main id="main-content" className="flex-1 min-h-0 overflow-hidden">
          <div key={viewKey} className="h-full min-h-0 animate-fade-in-opacity">
            <Suspense fallback={<MainSpinner />}>{children}</Suspense>
          </div>
        </main>
      </div>
    </div>
  );
}
