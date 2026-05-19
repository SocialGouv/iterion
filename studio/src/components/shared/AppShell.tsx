import { Suspense } from "react";
import type { ReactNode } from "react";

import Sidebar from "./Sidebar";
import ContextualHeaderBar from "./ContextualHeaderBar";
import MainSpinner from "./MainSpinner";
import TabBar from "./TabBar";
import { useTabLocationSync } from "@/hooks/useTabLocationSync";
import { useTabHotkeys } from "@/hooks/useTabHotkeys";
import { useUIStore } from "@/store/ui";

interface AppShellProps {
  children: ReactNode;
}

// AppShell is the persistent layout root for all authenticated routes.
// Sidebar, TabBar, and ContextualHeaderBar stay mounted across navigation;
// only <main> swaps its lazy-loaded route content. Phase 1 keeps the
// wouter <Switch> (passed in via children) as the actual renderer; the
// TabBar is a state/UI skin synced bidirectionally with the URL.
//
// Focus mode: when useUIStore.expanded is true (editor canvas-only
// mode), the chrome is dropped entirely — including the "Skip to
// main content" link, since there's nothing to skip past.
export default function AppShell({ children }: AppShellProps) {
  const expanded = useUIStore((s) => s.expanded);

  // Hooks must run unconditionally; both sync hooks no-op when their
  // dependencies haven't moved, so they're cheap to call on every
  // render — including focus mode.
  useTabLocationSync();
  useTabHotkeys();

  if (expanded) {
    return (
      <div className="h-screen w-screen bg-surface-0 text-fg-default">
        <Suspense fallback={<MainSpinner />}>{children}</Suspense>
      </div>
    );
  }

  return (
    <div className="h-screen w-screen flex bg-surface-0 text-fg-default overflow-hidden">
      <a
        href="#main-content"
        className="sr-only focus:not-sr-only focus:fixed focus:top-2 focus:left-2 focus:z-[var(--z-toast)] focus:bg-accent focus:text-fg-onAccent focus:px-3 focus:py-1.5 focus:rounded focus:shadow-lg"
      >
        Skip to main content
      </a>
      <Sidebar />
      <div className="flex-1 min-w-0 flex flex-col">
        <TabBar />
        <ContextualHeaderBar />
        <main id="main-content" className="flex-1 min-h-0 overflow-hidden">
          <Suspense fallback={<MainSpinner />}>{children}</Suspense>
        </main>
      </div>
    </div>
  );
}
