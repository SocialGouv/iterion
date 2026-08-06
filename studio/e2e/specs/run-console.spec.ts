import { expect, test } from "@playwright/test";

import { seed } from "../lib/state";

// studio-ui.run-console — the run console renders a REAL run the engine
// produced: its graph (node ids, kinds, per-node status), its persisted
// log lines, its budget/progress summary, and the published artifact
// payloads. Every string asserted below originates in the seeded run's
// own store files, so cutting any link in the chain
// (store → /api/runs → SPA) turns these red.

test("runs list shows the seeded run with its workflow and status", async ({
  page,
}) => {
  const { fixtureRunId } = seed();
  await page.goto("/runs");

  // The Run ID column truncates, so address the row by the prefix it
  // renders — unique even between two runs seeded in the same second.
  const row = page
    .getByRole("row")
    .filter({ hasText: fixtureRunId.slice(0, 13) });
  await expect(row).toHaveCount(1);
  await expect(row).toContainText("/demo-bot/main.bot");
  await expect(row).toContainText("finished");
});

test("run console renders the executed graph, log and outcome", async ({
  page,
}) => {
  const { fixtureRunId } = seed();
  await page.goto(`/runs/${fixtureRunId}`);

  // Header: the workflow the run executed and its terminal status.
  await expect(page.getByRole("button", { name: "ui_fixture" })).toBeVisible();
  await expect(page.getByTitle("Finished").first()).toBeVisible();

  // Graph: one node per IR node, each carrying the kind the DSL declared
  // and the status the engine persisted. `fail` was never reached, so it
  // must NOT read as finished.
  const collect = page.getByTestId("rf__node-collect_facts");
  await expect(collect).toContainText("tool");
  await expect(collect).toContainText("finished");
  await expect(collect).toContainText("entry");

  const decide = page.getByTestId("rf__node-decide");
  await expect(decide).toContainText("compute");
  await expect(decide).toContainText("finished");

  await expect(page.getByTestId("rf__node-done")).toContainText("finished");
  await expect(page.getByTestId("rf__node-fail")).not.toContainText("finished");

  // Overview panel: the budget the .bot declared (max_iterations: 5) and
  // the node count the run actually executed (collect_facts, decide, done).
  const overview = page.getByRole("complementary").filter({ hasText: "Budget" });
  await expect(overview).toContainText("/ 5");
  await expect(overview).toContainText("nodes");

  // Log panel: the engine's own event lines, replayed from events.jsonl.
  // The list is virtualized and follows the tail, so filter it down to the
  // routing decisions rather than scrolling — those are the two edges the
  // engine actually selected (the `fail` edge never fired).
  await page.getByRole("tab", { name: "Logs" }).click();
  await expect(page.getByText("Run finished")).toBeVisible();
  await page.getByRole("textbox", { name: "Search log…" }).fill("Edge:");
  await expect(page.getByText("Edge:  → decide")).toBeVisible();
  await expect(page.getByText("Edge:  → done (condition: ok)")).toBeVisible();
  await expect(page.getByText("→ fail")).toHaveCount(0);
});

test("run console surfaces a published artifact's real payload", async ({
  page,
}) => {
  const { fixtureRunId } = seed();
  await page.goto(`/runs/${fixtureRunId}`);

  await page.getByRole("tab", { name: "Artifacts" }).click();
  await expect(
    page.getByRole("heading", { name: "Published outputs" }),
  ).toBeVisible();

  // Both `publish:`ed nodes appear; expanding `decide` shows the exact
  // values the compute node produced and the store persisted.
  await expect(page.getByText("Collect facts", { exact: true })).toBeVisible();
  const decide = page.locator("details").filter({ hasText: "Decide" });
  await decide.locator("summary").click();
  await expect(decide).toContainText("the deterministic fixture converged");
  await expect(decide).toContainText("ok");
});
