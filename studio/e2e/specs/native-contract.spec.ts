import fs from "node:fs";
import { expect, test } from "@playwright/test";
import { wsPath } from "../lib/state";

test("native public graph and technical configuration survive a Studio edit", async ({ page }) => {
  const file = wsPath("bots", "native-contract", "main.bot");
  await page.goto("/editor?file=bots/native-contract/main.bot");

  await expect(page.getByTestId("rf__node-render")).toContainText("Render an item");
  await expect(page.getByTestId("rf__node-collect")).toContainText("Collect results");
  await expect(page.getByText("map each (dynamic)")).toBeVisible();
  await expect(page.getByTestId("public-contract")).toContainText("Render a batch");
  await expect(page.getByText("results ← collect.texts · product")).toBeVisible();
  await page.getByLabel("Input to supply").selectOption("collect.texts");
  await expect(page.getByLabel("Source output").locator('option[value="render.text"]')).toHaveText("render.text: string[]");

  await page.getByTestId("rf__node-render").click();
  await expect(page.getByTestId("public-contract")).toContainText("Prefix one item");
  await expect(page.getByTestId("public-contract")).toContainText("item");
  await expect(page.getByTestId("public-contract")).toContainText("text");
  const technical = page.getByText("Technical configuration");
  await technical.click();
  await expect(page.getByText("Implementation: render_impl")).toBeVisible();

  await page.getByText("Edit public contract").click();
  await page.getByRole("textbox", { name: "Responsibility" }).fill("Prefix one item deterministically");
  await page.getByRole("button", { name: "Save" }).click();
  await expect(async () => {
    const source = fs.readFileSync(file, "utf8");
    expect(source).toContain("Prefix one item deterministically");
    expect(source).toContain("compute render_impl:");
    expect(source).toContain("max_map_items: 4");
    expect(source).toContain("products: [\"results\"]");
  }).toPass();

  await page.goto("/runs");
  await page.goto("/editor?file=bots/native-contract/main.bot");
  await page.getByTestId("rf__node-render").click();
  await expect(page.getByTestId("public-contract")).toContainText("Prefix one item deterministically");
});
