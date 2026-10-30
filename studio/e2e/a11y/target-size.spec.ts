import type { Page } from "@playwright/test";
import { expect, test } from "@playwright/test";

import { studio } from "../lib/paths";
import { assertTargetSize, TARGET_SIZE_WAIVERS } from "./targetSize";

// studio-a11y.target-size — SC 2.5.8 (WCAG 2.2 AA) judged in a REAL engine.
//
// The jsdom axe net (src/__tests__/a11y) declares wcag22aa — whose only rule
// is `target-size` — and passes it outright, because without layout every
// element measures 0×0 and axe reads that as a pass (#2765). This spec is
// the other half: the same rule, in Chromium, over the shipped surfaces,
// with a verdict per surface and an explicit, justified waiver list
// (./targetSize.ts) separating a true sub-24px violation from a documented
// exception.
//
// ON DEMAND ONLY — the trigger (per-PR CI vs nightly vs manual) is pending
// with Jo (#2765), so this file lives outside playwright.config.ts's
// testDir. Run it:
//
//   devbox run -- task test:a11y:target-size     # builds the binary, skips
//                                                # cleanly without chromium
//   corepack pnpm -C studio test:a11y:target-size
//
// Both need the e2e prerequisites: the iterion binary (task build — the SPA
// is embedded at build time) and the Playwright chromium
// (task test:e2e:ui:install).

interface Surface {
  /** The name waivers reference and reports lead with. */
  name: string;
  /** Where the scanner navigates. */
  path: string;
  /** What proves the surface actually rendered — a scan of a half-booted
   *  page is a verdict on nothing. */
  ready: (page: Page) => Promise<void>;
}

// The declared surface list. A surface joins only with a comment saying why
// it is here; a surface leaves by deletion, never by commenting-out.
const SURFACES: Surface[] = [
  {
    // The runs list — the studio's landing surface after Home, standing in
    // for the pre-auth login page #2765's surface wishlist leads with: the
    // login view is a CLOUD-only screen (the local server holds an implicit
    // session, and Login.tsx bounces an authenticated visitor straight into
    // the studio — the rgaa demo's scope notes already list login among the
    // cloud-only screens its sample cannot reach). No local server mode
    // renders it, so the cheap clean surface here is the seeded runs list.
    name: "runs-list",
    path: studio("/runs"),
    ready: async (page) => {
      await expect(
        page.getByRole("row").filter({ hasText: "/demo-bot/main.bot" }),
      ).toContainText("finished");
    },
  },
  {
    // The launch form — the surface #2765 names: it renders VarFieldInput's
    // bool branch (the 14px checkbox) once the Bot options disclosure is
    // open. The known finding is waivered-and-documented in
    // TARGET_SIZE_WAIVERS; deleting that waiver must fail this test.
    name: "launch-form",
    path: studio("/runs/new?file=bots/a11y-bool-vars/main.bot"),
    ready: async (page) => {
      await expect(
        page.getByRole("heading", { name: "A11y Bool Vars" }),
      ).toBeVisible();
      // bool vars are never required (src/lib/varValidation.tsx), so the
      // fixture's `flag` lands in the optional disclosure — open it.
      await page.getByRole("button", { name: /^Bot options/ }).click();
      await expect(page.locator("#var-flag")).toBeVisible();
    },
  },
];

for (const surface of SURFACES) {
  test(`${surface.name}: every target meets SC 2.5.8 or carries a documented waiver`, async ({
    page,
  }) => {
    await page.goto(surface.path);
    await surface.ready(page);
    const scan = await assertTargetSize(page, surface.name);
    // The ledger is part of the verdict: print what was waived so a green
    // run still SHOWS its exceptions.
    for (const { geometry, waiver } of scan.waived) {
      console.log(
        `[target-size] ${surface.name}: waived ${geometry.selector} ` +
          `(${geometry.widthPx}×${geometry.heightPx}px, ${geometry.kind}) — ${waiver.justification}`,
      );
    }
  });
}

// A waiver naming a surface nobody scans is a dead claim — the same way a
// waiver matching no node is.
test("every waiver names a declared surface", () => {
  const names = new Set(SURFACES.map((s) => s.name));
  const orphans = TARGET_SIZE_WAIVERS.filter((w) => !names.has(w.surface));
  expect(orphans).toEqual([]);
});
