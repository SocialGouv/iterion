import { expect, test } from "@playwright/test";

// studio-ui.launch-modal — the Launch view builds its form from the
// workflow the server parsed: the bot's manifest identity, the `vars:`
// block split into required inputs vs optional ones, the per-node engine
// overrides, and the validation that blocks a launch with a required var
// missing.

const LAUNCH = "/runs/new?file=bots/launch-fixture/main.bot";

test("bot picker resolves ?bot= to the catalog entry's workflow", async ({
  page,
}) => {
  await page.goto("/runs/new?bot=launch-fixture");
  await expect(
    page.getByRole("heading", { name: "Launch Fixture" }),
  ).toBeVisible();
  await expect(
    page.getByText("bots/launch-fixture/main.bot").first(),
  ).toBeVisible();
});

test("launch form renders the declared vars in their disclosure buckets", async ({
  page,
}) => {
  await page.goto(LAUNCH);

  // Identity comes from the bot's manifest.yaml, read off the filesystem.
  await expect(
    page.getByRole("heading", { name: "Launch Fixture" }),
  ).toBeVisible();
  await expect(
    page.getByText(
      "An agent-bearing fixture the studio UI e2e suite renders in the Launch form.",
    ),
  ).toBeVisible();

  // `mission: string` (no default) is a required primary input…
  const mission = page.getByRole("textbox", { name: /^mission/ });
  await expect(mission).toBeVisible();
  await expect(mission).toHaveValue("");

  // …while `target_branch: string = "main"` is optional, folded into the
  // Bot options disclosure, pre-filled with the DSL default.
  await expect(mission).toBeVisible();
  await page.getByRole("button", { name: /^Bot options/ }).click();
  await expect(page.getByRole("textbox", { name: /^target_branch/ })).toHaveValue(
    "main",
  );
});

test("launch is blocked until the required var is supplied", async ({
  page,
}) => {
  await page.goto(LAUNCH);

  await expect(page.getByRole("status")).toContainText(
    "Required input missing: mission",
  );

  await page.getByRole("textbox", { name: /^mission/ }).fill("audit the fixture");
  await expect(page.getByText("Required input missing")).toHaveCount(0);
});

test("engine options expose the workflow's own LLM node for retargeting", async ({
  page,
}) => {
  await page.goto(LAUNCH);

  await page.getByRole("button", { name: /^Engine options/ }).click();
  await page.getByRole("button", { name: /Model & backend per node/ }).click();
  // `survey` is the only agent node the fixture declares; it must be
  // offered as a per-node model override target.
  await expect(page.getByText("survey", { exact: true }).first()).toBeVisible();
  // The workflow opts out of sandboxing, and the form says so.
  await expect(page.getByText("Sandbox: none")).toBeVisible();
});

// The model-capability caption reads the node's OWN model on the inherit
// path — the input is empty and the model lives only in the placeholder, which
// is most launches. Offline the spec aggregator contributes nothing, so the
// assertion is on the curated context window and on the source that says the
// answer can still improve; the published price needs a fetched table.
test("the per-node model picker captions its model's capabilities", async ({
  page,
}) => {
  await page.goto(LAUNCH);

  await page.getByRole("button", { name: /^Engine options/ }).click();
  await page.getByRole("button", { name: /Model & backend per node/ }).click();

  const caption = page.getByTestId("model-caps-caption").first();
  await expect(caption).toBeVisible();
  await expect(caption).toContainText("1M context");
  await expect(caption).toContainText("curated");
  // Zero is unknown, never free — the caption must never print a $0 rate.
  await expect(caption).not.toContainText("$0.00");
});

// A workflow with NO agent/judge node must still be launchable: the
// cost-preview endpoint answers `{"nodes": null, "notes": ["no_llm_nodes"]}`
// for it, and CostPreviewChip must tolerate the null and simply hide (the
// chip is decoration, never a gate). This suite originally caught the
// null-deref crashing the whole Launch view into its error boundary for
// any tool+compute-only bot (reproduced with bots/demo-bot).
test("a workflow with no LLM nodes still renders a launchable view", async ({
  page,
}) => {
  await page.goto("/runs/new?file=bots/demo-bot/main.bot");
  await expect(page.getByRole("button", { name: "Launch" })).toBeVisible();
  await expect(page.getByText("Launch view crashed")).toHaveCount(0);
});
