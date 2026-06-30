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
import App from "./App";
import "./app.css";
import { initializeTheme } from "./store/theme";
import { initializeBackendDetect } from "./store/backendDetect";
import { initializeServerInfo } from "./store/serverInfo";

initializeTheme();
initializeBackendDetect();
initializeServerInfo();

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
      <App />
    </QueryClientProvider>
  </StrictMode>,
);
