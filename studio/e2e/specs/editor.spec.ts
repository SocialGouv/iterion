import fs from "node:fs";

import { expect, test } from "@playwright/test";

import { wsPath } from "../lib/state";

// studio-ui.editor — the editor is a full parse → edit → unparse → write
// round-trip over the server: the canvas and inspector are built from the
// IR the Go parser produced (diagnostics included), and saving an
// inspector edit rewrites the .bot source on disk.

test("editor renders the parsed graph, inspector and compiler diagnostics", async ({
  page,
}) => {
  await page.goto("/editor?file=bots/demo-bot/main.bot");

  // Canvas nodes carry the kind the parser assigned and, for the tool
  // node, the command it declared.
  const collect = page.getByTestId("rf__node-collect_facts");
  await expect(collect).toContainText("tool");
  await expect(collect).toContainText("echo iterion-ui-fixt");
  await expect(collect).toContainText("entry");
  const decide = page.getByTestId("rf__node-decide");
  await expect(decide).toContainText("compute");
  await expect(decide).toContainText("2 expr");

  // Inspector reflects the workflow block the source declared.
  await expect(page.getByRole("textbox", { name: "Workflow Name" })).toHaveValue(
    "ui_fixture",
  );
  await expect(page.getByLabel("Entry Node")).toHaveValue("collect_facts");
  await expect(page.getByRole("spinbutton", { name: "Max Iterations" })).toHaveValue(
    "5",
  );

  // Diagnostics come from the real IR compiler: the fixture opts out of
  // sandboxing, which is C128.
  await expect(page.getByRole("button", { name: /^C128/ })).toBeVisible();
});

test("an inspector edit is unparsed back into the .bot file on save", async ({
  page,
}) => {
  const file = wsPath("bots", "demo-bot", "main.bot");
  expect(fs.readFileSync(file, "utf8")).toContain("max_iterations: 5");

  await page.goto("/editor?file=bots/demo-bot/main.bot");
  await page.getByRole("spinbutton", { name: "Max Iterations" }).fill("9");
  await page.getByRole("button", { name: "Save" }).click();

  // The round-trip's observable outcome is the rewritten source file.
  await expect(async () => {
    expect(fs.readFileSync(file, "utf8")).toContain("max_iterations: 9");
  }).toPass();

  // …and re-opening the file re-parses that source, not a cached document.
  await page.goto("/runs");
  await page.goto("/editor?file=bots/demo-bot/main.bot");
  await expect(page.getByRole("spinbutton", { name: "Max Iterations" })).toHaveValue(
    "9",
  );

  // Leave the fixture as the other specs expect it.
  await page.getByRole("spinbutton", { name: "Max Iterations" }).fill("5");
  await page.getByRole("button", { name: "Save" }).click();
  await expect(async () => {
    expect(fs.readFileSync(file, "utf8")).toContain("max_iterations: 5");
  }).toPass();
});
