import fs from "node:fs";

import { expect, test } from "@playwright/test";

import { wsPath } from "../lib/state";
import { studio } from "../lib/paths";

// studio-ui.source-view — the per-file Source view of a bot in SEVERAL files
// (#1227). Its unit tests stub the server and its route tests never open a
// browser, so nothing reddened when the picker, the `editable` computation or
// the refusal banner regressed (#1649). The fixture `bots/multi-bot` is a main
// importing three fragments, one of which holds a value the writer cannot
// reproduce — so the refusal is a real one, produced by the real server.

const MULTI = "bots/multi-bot/main.bot";

// The fixture's main carries `dsl: 2` on purpose. Profile 2 escapes every
// string, so the writer has no multi-line form left and merging
// `lib/refused.bot`'s two-line value FOLDS it — which is what makes the
// merged render refusable. Under profile 1 the merge keeps the backtick
// form, nothing is refused, and #1649's fifth assertion has nothing to
// assert: measured, `/api/unparse` with `flatten` answered `refused: null`.
//
// Keep the fixture's comment block SHORT. Monaco virtualises: only the
// visible lines are in the DOM, and seven extra header lines pushed the
// oracle below the fold and reddened a healthy render.

/** Open the bot's editor and reveal the Source view. It is a half-pane
 *  toggled from the toolbar, not a tab, and the toggle does not persist
 *  across navigations. */
async function openSourceView(page: import("@playwright/test").Page) {
  await page.goto(studio(`/editor?file=${MULTI}`));
  await page.getByRole("button", { name: "Toggle source view" }).click();
  await expect(page.getByTestId("source-view-file-picker")).toBeVisible();
}

test("the picker lists every file of the unit, and opens on the main", async ({ page }) => {
  await openSourceView(page);
  const picker = page.getByTestId("source-view-file-picker");

  // The order is the unit's: the main first, then the fragments in import
  // order, then the merged program.
  await expect(picker.locator("option")).toHaveText([
    "main.bot (main)",
    "lib/schemas.bot",
    "lib/nodes.bot",
    "lib/refused.bot",
    "Merged program of 4 files (read-only)",
  ]);
  await expect(picker).toHaveValue("main.bot");
});

test("each file renders its own text, and the merged program is read-only", async ({
  page,
}) => {
  await openSourceView(page);
  const picker = page.getByTestId("source-view-file-picker");
  const editor = page.locator(".monaco-editor").first();
  await expect(editor).toBeVisible({ timeout: 30_000 });

  await picker.selectOption("lib/schemas.bot");
  // A fragment shows ITS declarations and not the main's — the whole point
  // of rendering by provenance.
  await expect(page.locator(".view-lines")).toContainText("schema note", {
    timeout: 15_000,
  });
  await expect(page.locator(".view-lines")).not.toContainText("workflow ui_unit_fixture");
  await expect(page.getByRole("button", { name: "Edit", exact: true })).toBeVisible();

  await picker.selectOption("<merged>");
  await expect(page.getByTestId("source-view-unit-note")).toBeVisible();
  // The merged program, asserted by its CONTENT. Without this the half
  // passes when the merged render FAILS: the editor stays on the previously
  // selected file under a picker that says "Merged program of 4 files", and
  // the note plus the absent Edit are both still true.
  //
  // Two texts no single file carries together — the MAIN's header comment
  // and a schema from lib/schemas.bot — so only a merge shows both. Both sit
  // near the top: Monaco virtualises, and an oracle deeper in the program
  // reddens on a healthy render as soon as the file grows a few lines.
  const merged = page.locator(".view-lines");
  await expect(merged).toContainText("the main imports three fragments", { timeout: 15_000 });
  await expect(merged).toContainText("schema note");
  // One text cannot be split back into the files it came from, so there is
  // no Edit on the merged entry.
  await expect(page.getByRole("button", { name: "Edit", exact: true })).toHaveCount(0);
  // …and the picker is still USABLE. Bounding a row that holds a long
  // read-only note is easy to do by clipping the picker away instead —
  // measured at zero painted pixels, under a note saying "Pick a file above".
  const pickerBox = await picker.boundingBox();
  expect(pickerBox!.width).toBeGreaterThan(40);
  const ownsItself = await page.evaluate(
    ([x, y]) => document.elementFromPoint(x as number, y as number)?.tagName ?? "NONE",
    [pickerBox!.x + 10, pickerBox!.y + pickerBox!.height / 2],
  );
  expect(ownsItself).toBe("SELECT");
});

test("a file the writer cannot reproduce is shown as stored, with the reason and no Edit", async ({
  page,
}) => {
  await openSourceView(page);
  await page.getByTestId("source-view-file-picker").selectOption("lib/refused.bot");

  const banner = page.getByRole("status").filter({ hasText: /cannot be edited here/i });
  await expect(banner).toBeVisible({ timeout: 15_000 });
  await expect(banner).toContainText("#1612");
  // `editable = (!unit || perFile) && !refused` is the expression at risk:
  // drop the `!refused` term and the Edit button comes back over a text no
  // save would accept.
  await expect(page.getByRole("button", { name: "Edit", exact: true })).toHaveCount(0);

  // The text on screen is the file's own, not the writer's render of it.
  await expect(page.locator(".view-lines")).toContainText("echo second line", {
    timeout: 15_000,
  });
});

// The kind of defect only a browser sees: the file picker's wrapper is
// `position: relative`, so any width it takes beyond its label paints ABOVE
// the static Apply/Cancel buttons and swallows their clicks. Measured at
// 1280×720 before the fix: the select spanned x 635→883 and Apply sat at
// 790→842, entirely underneath. Playwright's own actionability check catches
// it too — as a 60 s timeout — but a hit test names it in one line.
test("the file picker does not cover the Apply and Cancel controls", async ({ page }) => {
  await openSourceView(page);
  await page.getByTestId("source-view-file-picker").selectOption("lib/nodes.bot");
  await page.getByRole("button", { name: "Edit", exact: true }).click();

  // The MECHANISM, and the assertion that does not depend on the viewport:
  // the picker must stay inside its label. It overflowed by 77 px at 1280
  // (248 px of select in a 171 px label), and since its wrapper is
  // `position: relative` the overflow painted above the static buttons.
  // Whether that overflow happens to reach a given button is a function of
  // four widths this test does not own, so assert the overflow itself.
  const overflow = await page
    .getByTestId("source-view-file-picker")
    .evaluate((el) => {
      const label = (el.closest("label") as HTMLElement).getBoundingClientRect();
      return Math.round(el.getBoundingClientRect().right - label.right);
    });
  expect(overflow, "the file picker overflows its label").toBeLessThanOrEqual(0);

  // …and the user-facing consequence at this suite's own viewport.
  const owner = await page.getByRole("button", { name: "Apply" }).evaluate((el) => {
    const r = el.getBoundingClientRect();
    return document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2)?.tagName ?? "NONE";
  });
  expect(owner, "Apply is covered by another element").toBe("BUTTON");

  // Leave edit mode without touching the fixture.
  await page.getByRole("button", { name: "Cancel" }).click();
});

/** Retype the fragment's last content line, keeping its indentation.
 *
 *  Three Monaco behaviours decide this shape, each measured here:
 *  - `insertText` does NOT consume a selection, so the old text has to be
 *    deleted first or the two end up side by side (the apply then answers
 *    422);
 *  - `insertText` DOES auto-indent a multi-line payload — a whole fragment
 *    inserted at once came back with its indentation accumulating 2 → 4 → 6
 *    → 10 → 14 — so only a single line may be inserted;
 *  - `Home`/`End` address the VISUAL line, and this view sets
 *    `wordWrap: "on"`, so the line being replaced must be short enough not
 *    to wrap in a half-width pane. That is why the fixture's value is short.
 */
async function replaceLastLine(page: import("@playwright/test").Page, line: string) {
  await page.locator(".monaco-editor").first().click();
  await page.keyboard.press("ControlOrMeta+End");
  await page.keyboard.press("ArrowUp");
  await page.keyboard.press("End");
  await page.keyboard.press("Shift+Home");
  await page.keyboard.press("Backspace");
  // insertText rather than type(): typing a quote makes Monaco auto-close it.
  await page.keyboard.insertText(line);
  // The buffer, asserted before Apply: a wrap or a selection that did not
  // take must fail HERE, naming the editing step, not 60 s later on a file
  // that simply never changed. `toContainText` and not a raw string compare:
  // Monaco renders runs of spaces as non-breaking spaces, and only
  // `toContainText` normalises them.
  await expect(page.locator(".view-lines")).toContainText(line);
}

/** Apply, wait for it to have LANDED, then Save. A successful Apply closes
 *  edit mode, so Edit coming back is the barrier; clicking Save straight
 *  after Apply races the apply round trip with nothing to observe. */
async function applyThenSave(page: import("@playwright/test").Page) {
  await page.getByRole("button", { name: "Apply" }).click();
  await expect(page.getByRole("button", { name: "Edit", exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Save" }).click();
}

test("editing one file and saving rewrites that file alone", async ({ page }) => {
  const nodes = wsPath("bots", "multi-bot", "lib", "nodes.bot");
  const main = wsPath("bots", "multi-bot", "main.bot");
  const schemas = wsPath("bots", "multi-bot", "lib", "schemas.bot");
  const refused = wsPath("bots", "multi-bot", "lib", "refused.bot");

  const before = {
    nodes: fs.readFileSync(nodes, "utf8"),
    main: fs.readFileSync(main, "utf8"),
    schemas: fs.readFileSync(schemas, "utf8"),
    refused: fs.readFileSync(refused, "utf8"),
  };
  expect(before.nodes).toContain(`summary: "'converged'"`);

  await openSourceView(page);
  await page.getByTestId("source-view-file-picker").selectOption("lib/nodes.bot");
  await expect(page.locator(".view-lines")).toContainText("compute decide", {
    timeout: 15_000,
  });

  await page.getByRole("button", { name: "Edit", exact: true }).click();
  await replaceLastLine(page, `summary: "'landed'"`);
  await applyThenSave(page);

  await expect(async () => {
    expect(fs.readFileSync(nodes, "utf8")).toContain(`summary: "'landed'"`);
  }).toPass();

  // The headline of the per-file save, and the one thing no unit test can
  // prove: the bot's other files are byte-identical.
  expect(fs.readFileSync(main, "utf8")).toBe(before.main);
  expect(fs.readFileSync(schemas, "utf8")).toBe(before.schemas);
  expect(fs.readFileSync(refused, "utf8")).toBe(before.refused);

  // Leave the fixture as the other tests expect it: one server and one
  // workspace are shared by the whole suite.
  await page.getByRole("button", { name: "Edit", exact: true }).click();
  await replaceLastLine(page, `summary: "'converged'"`);
  await applyThenSave(page);
  await expect(async () => {
    expect(fs.readFileSync(nodes, "utf8")).toBe(before.nodes);
  }).toPass();
});

// #1649's fifth acceptance assertion. Download on a bot in several files
// flattens to the MERGED program, and the merged text cannot carry a value
// its author wrote over several lines (#1612) — so the download is refused
// rather than handing over a `.bot` that is the same program and not the
// same file. The toast has to NAME the file, or the author is told "cannot
// be downloaded" about a bot of four files with no way to tell which one.
// `useDocumentFileOps.handleDownload` is otherwise unexercised in a browser.
test("Download over the merged program is refused, and the toast names the file", async ({
  page,
}) => {
  await page.goto(studio(`/editor?file=${MULTI}`));
  // Wait for the file to be OPEN before touching the menu. The File button
  // is in the toolbar and renders before `openFile` resolves; clicking
  // Download first finds `unit` still null, which downloads the empty
  // document with no toast at all — a red test for a reason that is not the
  // one under test. The entry node is the cheapest proof the unit landed.
  await expect(page.getByTestId("rf__node-collect_facts")).toBeVisible({ timeout: 30_000 });
  await page.getByRole("button", { name: "File menu" }).click();
  await page.getByRole("menuitem", { name: "Download as .bot" }).click();

  const toast = page.getByRole("status").filter({ hasText: /cannot be downloaded as \.bot source/i });
  await expect(toast).toBeVisible({ timeout: 15_000 });
  // The file whose lines the merged text no longer carries…
  await expect(toast).toContainText("lib/refused.bot");
  await expect(toast).toContainText("#1612");
  // …and why there is no file to hand over instead, which is what makes this
  // different from the single-file case that downloads the stored text.
  await expect(toast).toContainText("no single file to hand over");

  // Persistent: it carries the only explanation the author gets, so it must
  // not disappear on a timer while they are reading it.
  await page.waitForTimeout(6_000);
  await expect(toast).toBeVisible();
});
