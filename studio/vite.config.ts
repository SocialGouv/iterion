/// <reference types="vitest" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "path";

// Dev-server proxy target: the Go editor backend. Overridable via
// ITERION_STUDIO_BACKEND so a second dev instance (e.g. from a worktree)
// can proxy to a backend on a non-default port.
// The matching origin header value the Go server's loopback allowlist
// will accept (rewritten on every proxied request below).
const TARGET = process.env.ITERION_STUDIO_BACKEND ?? "http://localhost:4891";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    sourcemap: true,
    // Split heavy third-party deps into named chunks so the initial
    // download stays close to the per-route shell (which is now
    // React.lazy'd per route in App.tsx). Without manual chunks the
    // single index-*.js bundle was >3 MB minified (892 kB gzipped),
    // which dominated cold load on slow links. Each chunk maps to a
    // dep cluster that ships together — splitting finer doesn't help
    // because the chunks share runtime modules.
    //
    // Expressed as rolldown's `advancedChunks.groups` (Vite 8 bundles with
    // rolldown, whose `manualChunks` only accepts a function — the
    // name → module-list object form of Vite ≤7 is rejected). Each group's
    // `test` matches the same packages the old object listed.
    rollupOptions: {
      output: {
        advancedChunks: {
          groups: [
            { name: "reactflow", test: /[\\/]node_modules[\\/]@xyflow[\\/]react[\\/]/ },
            // No `monaco` group on purpose. Naming it made the editor a
            // first-class shared chunk, which the entry document then
            // modulepreloaded — so every page FETCHED 4.2 MB of JS and a
            // render-blocking 158 KB stylesheet for an editor it never
            // mounts, even though nothing imports it statically. Monaco now
            // has exactly one importer (src/lib/monacoInstance.ts, reached
            // only through the React.lazy wrappers in src/lib/monaco.tsx), so
            // the natural dynamic-import boundary already gives it its own
            // chunk — loaded when an editor first renders.
            {
              name: "radix",
              test: /[\\/]node_modules[\\/]@radix-ui[\\/]react-(dialog|icons|popover|tabs|tooltip)[\\/]/,
            },
          ],
        },
      },
    },
  },
  resolve: {
    // Array form, because ORDER decides: the "@" prefix rule matches
    // "@/lib/monaco" too, and an object's entries cannot express "the
    // specific one first". Under vitest the editor module resolves to a
    // stub — see src/lib/monaco.stub.ts.
    alias: [
      ...(process.env.VITEST
        ? [
            {
              find: /^@\/lib\/monaco$/,
              replacement: path.resolve(__dirname, "src/lib/monaco.stub.ts"),
            },
          ]
        : []),
      { find: "@", replacement: path.resolve(__dirname, "src") },
    ],
  },
  // Vitest config. Most tests are pure-function and run under Node;
  // component a11y + DOM tests (`src/__tests__/a11y/*` and any
  // `*.dom.test.tsx`) opt into jsdom via a per-file annotation:
  //   // @vitest-environment jsdom
  // Files without the annotation keep the fast Node environment.
  test: {
    environment: "node",
    include: ["src/**/*.test.ts", "src/**/*.test.tsx"],
  },
  // Pre-bundle the run-console deps at boot so Vite doesn't trip on
  // its own race when discovering them on-the-fly (the "file does not
  // exist in optimize deps directory" reload loop). These were added
  // in the Phase 4-7 run-console refonte; declaring them here keeps
  // the optimizer cache stable across dep upgrades.
  optimizeDeps: {
    include: ["react-resizable-panels", "react-virtuoso"],
  },
  server: {
    proxy: {
      "/api": {
        target: TARGET,
        changeOrigin: true,
        // WebSocket upgrade support — without this, /api/ws and
        // /api/ws/runs/{id} 404 against the Vite dev server because
        // Vite does not auto-proxy upgrades.
        ws: true,
        // The server enforces a loopback Origin allowlist on
        // state-changing endpoints AND on the WS upgrader. Vite's
        // `changeOrigin: true` only rewrites the Host header, not
        // Origin — so the browser's "http://localhost:5173" Origin
        // would otherwise be rejected with 403. Rewrite Origin to
        // match the proxy target so the dev experience matches what
        // the production same-origin build sees.
        configure: (proxy) => {
          proxy.on("proxyReq", (proxyReq) => {
            if (proxyReq.getHeader("origin")) {
              proxyReq.setHeader("origin", TARGET);
            }
          });
          proxy.on("proxyReqWs", (proxyReq) => {
            if (proxyReq.getHeader("origin")) {
              proxyReq.setHeader("origin", TARGET);
            }
          });
        },
      },
    },
  },
});
