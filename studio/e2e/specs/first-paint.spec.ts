import { expect, test } from "@playwright/test";

// studio-ui.first-paint — the editor must stay OFF the critical path.
//
// Monaco is ~4.2 MB of JS plus a 158 KB render-blocking stylesheet. It is
// bundled (not fetched from a CDN — see security-headers.spec.ts), which is
// the point; what must not happen is every page paying for it. A single
// static import from app chrome is enough to pull it onto the entry graph,
// and nothing else in the build would complain.
//
// Measured in the browser rather than read off the bundle: a chunk name
// appearing in the entry file may be a lazy `import()` specifier, so only the
// requests a real page actually issues settle it.

/** Bytes fetched per URL while loading `path`, keyed by request URL. */
async function assetsOnLoad(page: import("@playwright/test").Page, path: string) {
  const seen: string[] = [];
  page.on("request", (req) => seen.push(req.url()));
  await page.goto(path);
  await page.waitForLoadState("networkidle");
  return seen;
}

// `monacoInstance` is OUR module (src/lib/monacoInstance.ts) and the editor's
// sole importer, so its chunk is the honest oracle for "monaco was loaded" —
// and unlike a vendor chunk name it does not move when the bundler regroups.
// `editor.api` is monaco's own largest chunk, matched as a second witness.
const MONACO_ASSET = /\/assets\/(monacoInstance|editor\.api)-[A-Za-z0-9_-]+\.js$/;

test("the runs list does not download the editor", async ({ page }) => {
  const seen = await assetsOnLoad(page, "/runs");

  // The page really rendered — otherwise "no monaco" is trivially true.
  await expect(page.getByRole("row").filter({ hasText: "/demo-bot/main.bot" })).toContainText(
    "finished",
  );

  const monaco = seen.filter((u) => MONACO_ASSET.test(u));
  expect(
    monaco,
    "the runs list pulled the Monaco chunk — something on the entry graph imports @/lib/monaco statically",
  ).toEqual([]);
});

test("opening the editor does download it", async ({ page }) => {
  const seen = await assetsOnLoad(page, "/editor?file=bots/demo-bot/main.bot");
  await page.getByRole("button", { name: "Toggle source view" }).click();
  await expect(page.locator(".monaco-editor").first()).toBeVisible({ timeout: 30_000 });

  // The other half of the contract: lazy must still mean "arrives when
  // needed". A test that only asserted the absence above would pass on a
  // build where the editor never loads at all.
  await expect
    .poll(() => seen.filter((u) => MONACO_ASSET.test(u)).length, { timeout: 30_000 })
    .toBeGreaterThan(0);
});
