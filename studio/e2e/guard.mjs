// Exits 0 when a Playwright chromium build is present, 1 otherwise.
// `task test:e2e:ui` uses it to skip cleanly (with the install hint)
// instead of failing on a machine that never ran `playwright install`.

import { existsSync } from "node:fs";
import { chromium } from "@playwright/test";

let exe = "";
try {
  exe = chromium.executablePath();
} catch {
  exe = "";
}

if (!exe || !existsSync(exe)) {
  console.error(
    "[studio-e2e] no Playwright chromium found — install it with:\n" +
      "  corepack pnpm -C studio exec playwright install chromium",
  );
  process.exit(1);
}
