import { expect, test } from "@playwright/test";

import { seed } from "../lib/state";

// studio-ui.board — the `/board` kanban renders the native tracker's real
// columns and cards, and moving a card through the UI persists to the
// native store (asserted through the REST surface AND a reload, so a
// purely optimistic client update would be caught).

test("board renders the native tracker's columns and the seeded card", async ({
  page,
}) => {
  const { issueId } = seed();
  await page.goto("/board");

  // Columns come from the native board config, not from the SPA.
  for (const state of ["Inbox", "Backlog", "Ready", "In progress", "Done"]) {
    await expect(page.getByText(state, { exact: true })).toBeVisible();
  }

  const card = page.getByRole("button", { name: "Fixture card in inbox" });
  await expect(card).toBeVisible();
  // The card carries the label the CLI created it with and its short id.
  await expect(card).toContainText("ui-e2e");
  await expect(card).toContainText(issueId.replace("native:", "").slice(0, 10));
});

test("moving a card in the UI persists to the native store", async ({
  page,
  request,
}) => {
  const { issueId } = seed();
  await page.goto("/board");

  // Selecting the card reveals the bulk-move toolbar; moving it there is
  // the deterministic equivalent of the drag-and-drop gesture (same
  // mutation, same endpoint).
  await page.getByRole("button", { name: "Fixture card in inbox" }).click();
  // Backlog, not an eligible column: the dispatcher spec starts a real
  // dispatcher later in the run, and a card parked in a claimable state
  // would be launched out from under the other specs' assertions.
  await page.getByLabel("Bulk move to column").selectOption("Backlog");

  // The store — not the client — is the source of truth.
  await expect(async () => {
    const res = await request.get(
      `/api/v1/native/issues/${encodeURIComponent(issueId)}`,
    );
    expect(res.ok()).toBeTruthy();
    expect((await res.json()).state).toBe("backlog");
  }).toPass();

  // And a fresh load reads it back from that store: the column counters
  // move with the card, and the per-column "select all" affordance (which
  // only renders for a non-empty column) follows it.
  await page.reload();
  const header = (column: string) =>
    page
      .locator("div")
      .filter({
        has: page.getByRole("button", { name: `Manage ${column} column` }),
      })
      .last();
  await expect(header("Backlog")).toContainText("1");
  await expect(header("Inbox")).toContainText("0");
  await expect(page.getByLabel("Select all in Backlog")).toBeVisible();
  await expect(page.getByLabel("Select all in Inbox")).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Fixture card in inbox" }),
  ).toBeVisible();
});
