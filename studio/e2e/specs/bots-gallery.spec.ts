import fs from "node:fs";

import { expect, test } from "@playwright/test";

import { wsPath } from "../lib/state";

// studio-ui.bots-gallery — `/bots` lists what botregistry discovered on
// disk, `/bots/:name` renders that bot's manifest, and `/bots/new` runs
// the guided builder all the way to a bundle written to the filesystem.

test("gallery lists the discovered bots with their manifest metadata", async ({
  page,
}) => {
  await page.goto("/bots");

  const demo = page.getByRole("button", { name: /Demo Fixture/ });
  await expect(demo).toBeVisible();
  await expect(demo).toContainText("demo-bot");
  await expect(demo).toContainText(
    "A deterministic tool+compute fixture used by the studio UI e2e suite.",
  );
  await expect(page.getByRole("button", { name: /Launch Fixture/ })).toBeVisible();

  // The search box filters the discovered set, not a hardcoded list.
  await page.getByRole("searchbox", { name: "Search bots" }).fill("launch");
  await expect(page.getByRole("button", { name: /Demo Fixture/ })).toHaveCount(0);
  await expect(page.getByRole("button", { name: /Launch Fixture/ })).toBeVisible();
});

test("bot home renders the manifest the registry read off disk", async ({
  page,
}) => {
  await page.goto("/bots/demo-bot");

  await expect(page.getByRole("heading", { name: "Demo Fixture" })).toBeVisible();
  await expect(page.getByText("v0.1.0")).toBeVisible();
  await expect(
    page.getByRole("textbox", { name: "Persona name (display_name)" }),
  ).toHaveValue("Demo Fixture");
  await expect(page.getByRole("textbox", { name: "Version" })).toHaveValue(
    "0.1.0",
  );
  // The workflow declares no vars, and the page says exactly that.
  await expect(page.getByText("This workflow declares no vars.")).toBeVisible();
});

test("guided builder writes a real bundle to the workspace", async ({
  page,
}) => {
  await page.goto("/bots/new");

  await page.getByRole("button", { name: /^Blank bot/ }).click();
  await page.getByRole("textbox", { name: "Name", exact: true }).fill("Scaffold Probe");
  await page
    .getByRole("textbox", { name: "Description", exact: true })
    .fill("Created by the studio UI e2e suite.");

  const create = page.getByRole("button", { name: "Create bot" });
  await expect(create).toBeEnabled();
  await create.click();

  // The builder's output is a bundle on disk — that is the observable
  // contract, not a toast.
  const dir = wsPath("bots", "scaffold-probe");
  await expect(async () => {
    expect(fs.existsSync(`${dir}/main.bot`)).toBeTruthy();
    expect(fs.existsSync(`${dir}/manifest.yaml`)).toBeTruthy();
  }).toPass();
  expect(fs.readFileSync(`${dir}/manifest.yaml`, "utf8")).toContain(
    "Scaffold Probe",
  );

  // …and the registry picks it up on the next gallery load.
  await page.goto("/bots");
  await expect(page.getByRole("button", { name: /Scaffold Probe/ })).toBeVisible();
});
