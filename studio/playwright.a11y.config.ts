import { defineConfig } from "@playwright/test";

import base from "./playwright.config";

// The real-engine a11y harness (#2765) — SC 2.5.8 target-size, which jsdom
// cannot judge. Same webServer (e2e/serve.mjs: the real binary, the seeded
// throwaway store), same browser; a DIFFERENT testDir so the specs stay out
// of the default `playwright test` sweep: the trigger (per-PR CI, nightly,
// on demand) is pending with Jo, and landing them in e2e/specs would decide
// it by side effect.
export default defineConfig({
  ...base,
  testDir: "./e2e/a11y",
});
