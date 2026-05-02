import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "path";

// Dev-server proxy target: the Go editor backend.
// The matching origin header value the Go server's loopback allowlist
// will accept (rewritten on every proxied request below).
const TARGET = "http://localhost:4891";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    sourcemap: true,
  },
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "src"),
    },
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
