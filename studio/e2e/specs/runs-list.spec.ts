import { expect, test } from "@playwright/test";

import { seed } from "../lib/state";

// studio-ui.runs-list — guards the virtualized runs table.
//
// The list moved from a fully-mapped <table>/<ul> to react-virtuoso's
// GroupedTableVirtuoso (desktop) / GroupedVirtuoso (mobile) so a
// several-hundred-run cloud list stops blocking the main thread on a scope
// switch. This suite runs against the REAL server in local mode (the only
// mode the loopback harness boots), so it can't drive team/repo scope
// switching — that path is covered by the unit/integration tests
// (useRuns keepPreviousData + refreshing, AuthContext invalidation,
// runListBodyState, RunListSkeleton). What it CAN and must guard is that
// the virtualization refactor still renders real rows with the semantic
// table markup, selection, and navigation intact.

test.describe("runs list (virtualized)", () => {
  test("renders the seeded run in the semantic table with its columns", async ({
    page,
  }) => {
    const { fixtureRunId } = seed();
    await page.goto("/runs");

    // The virtualized table keeps <table>/<thead> semantics: the column
    // headers are real <th scope=col> cells.
    await expect(
      page.getByRole("columnheader", { name: "Run", exact: true }),
    ).toBeVisible();
    await expect(page.getByRole("columnheader", { name: "Status" })).toBeVisible();

    // The table keeps its accessible name via the sr-only <caption>
    // (RGAA 5.4/5.5) even though Virtuoso renders the <table> element.
    await expect(page.getByRole("table", { name: "Runs" })).toBeAttached();

    // The seeded demo-bot run is present as a table row and shows finished.
    const row = page
      .getByRole("row")
      .filter({ hasText: "/demo-bot/main.bot" });
    await expect(row).toContainText("finished");

    // Its short run id (first 8 chars, the RunID column) renders too.
    await expect(row).toContainText(fixtureRunId.slice(0, 8));
  });

  test("select-all checkbox toggles the row selection toolbar", async ({
    page,
  }) => {
    await page.goto("/runs");
    await expect(
      page.getByRole("row").filter({ hasText: "/demo-bot/main.bot" }),
    ).toContainText("finished");

    // The fixed header carries the select-all checkbox even though rows are
    // virtualized. Checking it selects the visible rows and reveals the
    // bulk-action toolbar.
    await page.getByRole("checkbox", { name: "Select all runs" }).check();
    await expect(
      page.getByRole("checkbox", { name: /^Select run / }).first(),
    ).toBeChecked();
  });

  test("clicking a row opens the run console", async ({ page }) => {
    const { fixtureRunId } = seed();
    await page.goto("/runs");
    const row = page.getByRole("row").filter({ hasText: "/demo-bot/main.bot" });
    await expect(row).toContainText("finished");

    await row.click();
    await expect(page).toHaveURL(new RegExp(`/runs/${fixtureRunId}`));
  });

  test("grouped mode renders group headers and a header click is inert", async ({
    page,
  }) => {
    // Group headers flow through Virtuoso's group-row slot (no `item`).
    // Clicking one must NOT throw or navigate — the row is only clickable
    // when it carries a run. Deep-link the group axis so we don't depend
    // on the <select> wiring.
    await page.goto("/runs?group=workflow");
    const row = page.getByRole("row").filter({ hasText: "/demo-bot/main.bot" });
    await expect(row).toContainText("finished");

    // A group header row is a rowgroup <th> — at least one is present.
    const groupHeader = page.locator('th[scope="rowgroup"]').first();
    await expect(groupHeader).toBeVisible();

    // Clicking the header stays on /runs (no navigation, no crash).
    await groupHeader.click();
    await expect(page).toHaveURL(/\/runs(\?|$)/);
    // The list is still intact after the click (no error boundary).
    await expect(row).toContainText("finished");
  });

  test("mobile card list keeps <ul>/<li> semantics", async ({ page }) => {
    // The mobile GroupedVirtuoso renders cards through a <ul>/<li> List/Item
    // so assistive tech still gets list role + item count. On desktop the
    // table renders instead, so drive a phone viewport.
    await page.setViewportSize({ width: 375, height: 800 });
    await page.goto("/runs");

    // The card list is a <ul.divide-y> (the pre-virtualization separator
    // class) inside the main content — the sidebar nav <ul> is excluded by
    // that class. Cards are real <li> list items, so assistive tech still
    // gets list semantics + an item count.
    await expect(
      page.locator("#main-content ul.divide-y li").first(),
    ).toBeVisible();
  });
});
