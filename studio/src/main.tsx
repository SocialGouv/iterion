import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@xyflow/react/dist/style.css";
// Self-hosted variable fonts (no CDN — offline-safe for the desktop app and
// sandboxed runs). Geist (sans) for UI, Geist Mono for technical identifiers
// (run-ids / SHAs / node-ids) — see docs/visual-identity.md § Typography.
// Each declares `@font-face` with unicode-range subsets, so the browser only
// fetches the Latin woff2 for an English UI. Wired to the `--font-sans` /
// `--font-mono` tokens in app.css.
import "@fontsource-variable/geist";
import "@fontsource-variable/geist-mono";
import { Router } from "wouter";
import App from "./App";
import WorkspaceShell from "./workspace/WorkspaceShell";
import "./app.css";
import { initializeTheme } from "./store/theme";
import { initializeBackendDetect } from "./store/backendDetect";
import { initializeServerInfo } from "./store/serverInfo";
import { isWailsHosted } from "./lib/desktopBridge";
import { isScopedPane, scopePrefix } from "./lib/scope";

// Desktop workspace shell vs. studio app. The DESKTOP main frame (a Wails
// origin with NO scope) renders the WorkspaceShell — the tab/split chrome that
// hosts one scoped <iframe> pane per open connection. A PANE (Wails origin WITH
// a scope) and plain BROWSER mode both render the normal studio App, the pane
// routed under its /x/<id> base. isWailsHosted() is synchronous (origin check),
// so the branch is stable at first paint even before window.go finishes
// injecting — unlike isDesktop(), which would briefly read false.
const isWorkspaceShell = isWailsHosted() && !isScopedPane();

initializeTheme();
// The workspace shell talks to no single backend, so skip the boot-time
// backend/server probes (they'd hit the legacy /api primary); each pane runs
// its own probes scoped to its connection.
if (!isWorkspaceShell) {
  initializeBackendDetect();
  initializeServerInfo();
}

// Single QueryClient for the whole SPA. Defaults tuned for an
// interactive studio:
//  - staleTime 0 by default so polling hooks always see fresh data;
//    long-lived caches (capabilities, server info) override per-query.
//  - retry: 1 so a transient daemon hiccup doesn't bubble straight to
//    the UI without a chance to recover.
//  - refetchOnWindowFocus disabled because the run console and the
//    board already react to WebSocket / event-driven invalidations;
//    a blanket window-focus refetch would double-spam those flows.
const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 0,
      retry: 1,
      refetchOnWindowFocus: false,
    },
  },
});

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      {isWorkspaceShell ? (
        <WorkspaceShell />
      ) : (
        <Router base={scopePrefix()}>
          <App />
        </Router>
      )}
    </QueryClientProvider>
  </StrictMode>,
);
