import { defineConfig, devices } from "@playwright/test";

// Studio UI end-to-end suite. Every spec drives the REAL iterion server
// (the built binary serving the embedded SPA) against a throwaway store
// seeded with real runs and board cards — no LLM credential, no network
// beyond the loopback server itself.
//
// The port is fixed so the config (loaded per worker) and e2e/serve.mjs
// agree without a handshake file. Override with ITERION_UI_PORT when
// 4899 is taken.
const PORT = Number(process.env.ITERION_UI_PORT ?? 4899);

export default defineConfig({
  testDir: "./e2e/specs",
  // One server, one store: the specs mutate shared state (board cards,
  // dispatcher lifecycle, scaffolded bots), so they run sequentially.
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: 0,
  reporter: process.env.CI ? "line" : "list",
  timeout: 60_000,
  expect: { timeout: 15_000 },
  use: {
    baseURL: `http://127.0.0.1:${PORT}`,
    trace: "retain-on-failure",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer: {
    command: "node e2e/serve.mjs",
    url: `http://127.0.0.1:${PORT}/api/server/info`,
    // Never adopt a stray studio on this port: the specs assert against
    // the seeded fixture store, so a foreign server must fail loudly.
    reuseExistingServer: false,
    stdout: "pipe",
    stderr: "pipe",
    timeout: 120_000,
    env: { ITERION_UI_PORT: String(PORT) },
  },
});
