import fs from "node:fs";

import { expect, test } from "@playwright/test";

import { wsPath } from "../lib/state";

// studio-ui.secrets-view — the local Secrets view, gated on
// server_info.secrets_enabled, manages the sealed on-disk store. The
// assertions are the sealed file the server wrote (never the plaintext,
// which the UI is specified never to show again) and the row the view
// reads back from it.
//
// serve.mjs points ITERION_HOME/HOME at the throwaway workspace and pins
// ITERION_SECRETS_KEY, so this never reads or writes the operator's own
// ~/.iterion/secrets.json or their keychain.

const SECRET_NAME = "UI_E2E_TOKEN";
const SECRET_VALUE = "s3cr3t-value-1234";

test("the view is exposed and starts from the empty isolated store", async ({
  page,
  request,
}) => {
  // The gate itself: the route only renders because the server reported
  // a sealed local store.
  const info = await (await request.get("/api/server/info")).json();
  expect(info.secrets_enabled).toBe(true);

  await page.goto("/secrets");
  await expect(page.getByRole("heading", { name: "Secrets" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Add secret" })).toBeVisible();
  // The isolated store starts empty — proof the suite is not looking at
  // the operator's own ~/.iterion.
  await expect(page.getByText("No secrets yet")).toBeVisible();
});

test("adding a secret seals it on disk and lists it without the value", async ({
  page,
}) => {
  await page.goto("/secrets");

  await page.getByRole("button", { name: "Add secret" }).click();
  await page.getByPlaceholder("GITHUB_TOKEN").fill(SECRET_NAME);
  await page
    .getByPlaceholder("paste here — never shown again")
    .fill(SECRET_VALUE);
  await page.getByPlaceholder("github.com, api.github.com").fill("example.test");
  await page.getByRole("button", { name: "Create" }).click();

  // The row the view reads back carries the metadata, the egress host and
  // only the last characters of the value.
  const row = page.getByRole("row").filter({ hasText: SECRET_NAME });
  await expect(row).toBeVisible();
  await expect(row).toContainText("global");
  await expect(row).toContainText("example.test");
  await expect(row).not.toContainText(SECRET_VALUE);

  // The store on disk holds it SEALED — the plaintext must not be there.
  const file = wsPath("home", "secrets.json");
  await expect(async () => {
    expect(fs.existsSync(file)).toBeTruthy();
    const raw = fs.readFileSync(file, "utf8");
    expect(raw).toContain(SECRET_NAME);
    expect(raw).not.toContain(SECRET_VALUE);
  }).toPass();
});

test("deleting a secret removes it from the store", async ({ page }) => {
  await page.goto("/secrets");
  const row = page.getByRole("row").filter({ hasText: SECRET_NAME });
  await expect(row).toBeVisible();

  await row.getByRole("button", { name: "Delete" }).click();
  await page
    .getByRole("dialog", { name: `Delete ${SECRET_NAME}?` })
    .getByRole("button", { name: "Delete", exact: true })
    .click();

  await expect(page.getByText("No secrets yet")).toBeVisible();
  const file = wsPath("home", "secrets.json");
  await expect(async () => {
    expect(fs.readFileSync(file, "utf8")).not.toContain(SECRET_NAME);
  }).toPass();
});
