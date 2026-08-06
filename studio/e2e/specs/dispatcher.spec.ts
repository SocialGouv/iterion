import { expect, test } from "@playwright/test";

import { wsPath } from "../lib/state";

// studio-ui.dispatcher — the `/dispatcher` dashboard is a live view onto
// the dispatcher actor the server owns: it configures one, starts it, and
// renders that instance's real settings and lane counters. The spec drives
// the whole lifecycle (configure → start → observe → stop).
//
// The poll interval is pinned to an hour so the dispatcher never actually
// claims the seeded board card mid-test — the dashboard, not the polling
// loop, is what this row is about.

const POLL_MS = 3_600_000;

test("configure → start → stop, with the dashboard reflecting the instance", async ({
  page,
}) => {
  await page.goto("/dispatcher");

  // Nothing attached yet, and Start is correctly refused without a config.
  await expect(page.getByTitle(/No dispatcher attached/)).toContainText(
    "dispatcher: idle",
  );
  await expect(page.getByRole("button", { name: "Start the dispatcher" })).toBeDisabled();

  await page.getByRole("button", { name: "Dispatcher settings" }).click();
  await page
    .getByRole("textbox", { name: /^Name/ })
    .fill("ui-e2e-dispatcher");
  await page
    .getByRole("textbox", { name: /^Workflow path/ })
    .fill(wsPath("bots", "demo-bot", "main.bot"));
  await page.getByRole("spinbutton", { name: /^Interval \(ms\)/ }).fill(String(POLL_MS));
  await page.getByRole("button", { name: "Save" }).click();

  // A saved config is what unlocks Start.
  const start = page.getByRole("button", { name: "Start the dispatcher" });
  await expect(start).toBeEnabled();
  await start.click();

  // The dashboard now renders the RUNNING instance's own settings.
  await expect(page.getByText("dispatcher: running")).toBeVisible();
  await expect(
    page.getByRole("heading", { name: "ui-e2e-dispatcher" }),
  ).toBeVisible();
  const summary = page.getByRole("definition");
  await expect(summary.filter({ hasText: "native" })).toBeVisible();
  await expect(summary.filter({ hasText: `${POLL_MS / 1000}s` })).toBeVisible();
  // Concurrency slots come from the config's max_concurrent (default 2).
  await expect(summary.filter({ hasText: "0 / 2" })).toBeVisible();
  await expect(page.getByText("Running (0)")).toBeVisible();
  await expect(page.getByText("Retry queue (0)")).toBeVisible();

  // Stopping is confirmed, then detaches it and the dashboard falls back
  // to the empty state.
  await page.getByRole("button", { name: "Stop the dispatcher" }).click();
  await page
    .getByRole("dialog", { name: "Stop the dispatcher?" })
    .getByRole("button", { name: "Stop", exact: true })
    .click();
  await expect(page.getByText("No dispatcher running")).toBeVisible();
});

test("the settings dialog surfaces the server's own config rejection", async ({
  page,
}) => {
  await page.goto("/dispatcher");
  await page.getByRole("button", { name: "Dispatcher settings" }).click();
  await page.getByRole("textbox", { name: /^Name/ }).fill("bad-config");
  await page
    .getByRole("textbox", { name: /^Workflow path/ })
    .fill("does/not/exist.bot");
  await page.getByRole("button", { name: "Save" }).click();

  // The banner is the server's validation error, not a client-side guess.
  await expect(page.getByRole("alert")).toContainText("does/not/exist.bot");
  await expect(page.getByRole("alert")).toContainText("no such file or directory");
});
