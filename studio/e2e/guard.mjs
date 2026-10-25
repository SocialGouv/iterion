// 0: Chromium is installed; 77: an explicit local skip; 1: missing in CI.
// Only 77 may be translated into a successful task. Unexpected guard errors
// must fail, and CI must never claim coverage without a browser.

import { appendFileSync, existsSync } from "node:fs";
import { chromium } from "@playwright/test";

const exe = chromium.executablePath();

if (!exe || !existsSync(exe)) {
  const ci = !!process.env.CI || process.env.GITHUB_ACTIONS === "true";
  const message =
    `[studio-e2e] ${ci ? "ERROR" : "SKIPPED"}: no Playwright Chromium browser installed; ` +
    "the Studio UI e2e suite did not run. " +
    "Install it with: devbox run -- task test:e2e:ui:install";
  console.error(message);
  if (process.env.GITHUB_ACTIONS === "true") {
    console.error(`::error title=Studio UI e2e did not run::${message}`);
  }
  if (process.env.GITHUB_STEP_SUMMARY) {
    appendFileSync(process.env.GITHUB_STEP_SUMMARY, `${message}\n`);
  }
  process.exit(ci ? 1 : 77);
}
