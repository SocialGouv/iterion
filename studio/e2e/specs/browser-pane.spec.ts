import { expect, test } from "@playwright/test";

import { seed } from "../lib/state";

// studio-ui.browser-pane — the run console's Browser pane is
// level-triggered off the run's own `preview_url_available` events: a tool
// node printed `[iterion] preview_url=…`, the runtime turned that into an
// event, and the pane appears (and auto-reveals) carrying that URL. The
// seeded URL is the suite's own loopback origin, so the pane's iframe
// never leaves the test server.

test("a workflow-published preview URL reveals the Browser pane", async ({
  page,
}) => {
  const { previewRunId, previewUrl } = seed();
  await page.goto(`/runs/${previewRunId}`);

  // The pane is revealed automatically the first time a preview URL
  // becomes available, so no click is needed to make the tab appear.
  const tab = page.getByRole("tab", { name: "Browser" });
  await expect(tab).toBeVisible();
  await expect(tab).toHaveAttribute("aria-selected", "true");

  // The URL bar holds exactly what the workflow published…
  await expect(
    page.getByRole("textbox", {
      name: "Enter URL or wait for the workflow to publish one",
    }),
  ).toHaveValue(previewUrl);
  // …and the escape-hatch link points at the same place.
  await expect(page.getByRole("link", { name: "open ↗" })).toHaveAttribute(
    "href",
    previewUrl,
  );
});

test("a run that published no preview URL has no Browser pane", async ({
  page,
}) => {
  const { fixtureRunId } = seed();
  await page.goto(`/runs/${fixtureRunId}`);

  // Discriminates the pane's trigger from "the tab is always there":
  // the tool+compute fixture emits no preview_url_available event.
  await expect(page.getByRole("tab", { name: "Logs" })).toBeVisible();
  await expect(page.getByRole("tab", { name: "Browser" })).toHaveCount(0);
});
