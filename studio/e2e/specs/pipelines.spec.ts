import { expect, test } from "@playwright/test";

import { seed } from "../lib/state";

// studio-ui.pipelines — the `/pipelines` control centre renders the
// concurrency cap the server actually booted with (`--max-concurrent-
// pipelines`, surfaced on /api/server/info) and files real runs into its
// live / opened / closed lanes.

test("pipeline board renders the server's concurrency cap", async ({ page }) => {
  const { maxConcurrentPipelines } = seed();
  await page.goto("/pipelines");

  // The banner is fed by server_info.pipeline_concurrency — hardcoding a
  // different cap in serve.mjs must turn this red.
  await expect(
    page.getByTitle(`Concurrency cap: ${maxConcurrentPipelines}`),
  ).toContainText(`max ${maxConcurrentPipelines}`);
  await expect(
    page.getByTitle(`Concurrency cap: ${maxConcurrentPipelines}`),
  ).toContainText("0 running");

  // Nothing is live in the seeded store, and the board says so rather
  // than rendering a phantom card.
  await expect(page.getByRole("heading", { name: "In progress" })).toBeVisible();
  await expect(page.getByText("Nothing running right now.")).toBeVisible();
});

test("closed inventory lists the finished runs as pipeline cards", async ({
  page,
}) => {
  const { fixtureRunId } = seed();
  await page.goto("/pipelines");

  // Two seeded runs finished, so the Closed tab carries both and Opened
  // is empty.
  await expect(page.getByRole("tab", { name: /^Opened/ })).toContainText("0");
  const closed = page.getByRole("tab", { name: /^Closed/ });
  await expect(closed).toContainText("2");
  await closed.click();

  const card = page.getByRole("article", { name: /Demo Fixture/ });
  await expect(card).toContainText("Success");
  // The card links to the run console for the run it represents.
  await expect(
    card.getByRole("link", {
      name: `Open run ${fixtureRunId} in the run console`,
    }),
  ).toHaveAttribute("href", `/runs/${fixtureRunId}`);
});
