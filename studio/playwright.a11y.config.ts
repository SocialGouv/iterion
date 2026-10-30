import { defineConfig } from "@playwright/test";

import base from "./playwright.config";

// The real-engine a11y harness (#2765) — SC 2.5.8 target-size, which jsdom
// cannot judge. Same webServer (e2e/serve.mjs: the real binary, the seeded
// throwaway store), same browser; a DIFFERENT testDir so the specs stay out
// of the default `playwright test` sweep: the decided trigger is the
// studio-e2e-advisory workflow (schedule + workflow_dispatch), never the
// per-PR gate.
export default defineConfig({
  ...base,
  testDir: "./e2e/a11y",
});
