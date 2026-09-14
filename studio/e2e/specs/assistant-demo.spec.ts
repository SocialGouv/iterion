import { expect, test, type APIRequestContext, type Page } from "@playwright/test";

import { seed } from "../lib/state";

const DEMO_ENABLED = process.env.ITERION_COPI_DEMO === "1";
const rawDelay = Number(process.env.ITERION_E2E_DEMO_DELAY_MS ?? "0");
if (!Number.isFinite(rawDelay) || rawDelay < 0) {
  throw new Error("ITERION_E2E_DEMO_DELAY_MS must be a non-negative number");
}
const DEMO_DELAY_MS = Math.min(Math.floor(rawDelay), 3_000);
const VISUAL_STEPS = 8;
const WATCH_TIMEOUT_MS = 90_000;

async function presentationPause(page: Page) {
  if (DEMO_DELAY_MS > 0) await page.waitForTimeout(DEMO_DELAY_MS);
}

async function mockState(request: APIRequestContext) {
  const controlUrl = seed().mockOpenAIControlUrl;
  if (!controlUrl) throw new Error("Copi demo mock is not enabled");
  const response = await request.get(`${controlUrl}/state`);
  expect(response.ok()).toBeTruthy();
  return response.json() as Promise<{
    mode: "fail" | "succeed";
    completionRequests: number;
  }>;
}

async function runStatus(request: APIRequestContext, runId: string) {
  const response = await request.get(`${seed().origin}/api/runs/${runId}`);
  if (!response.ok()) return `http-${response.status()}`;
  const snapshot = (await response.json()) as { run?: { status?: string } };
  return snapshot.run?.status ?? "missing";
}

test.skip(!DEMO_ENABLED, "run through task demo:copi or set ITERION_COPI_DEMO=1");

test("@demo Copi crée, lance, surveille et corrige un workflow", async ({
  page,
  request,
}) => {
  test.setTimeout(300_000 + DEMO_DELAY_MS * VISUAL_STEPS);

  await test.step("Ouvrir Copi et formuler la mission", async () => {
    const controlUrl = seed().mockOpenAIControlUrl;
    if (!controlUrl) throw new Error("Copi demo mock is not enabled");
    const reset = await request.post(`${controlUrl}/reset`);
    expect(reset.ok()).toBeTruthy();

    await page.goto("/runs");
    await page.getByRole("button", { name: "Open assistant" }).click();
    const dock = page.getByRole("dialog", { name: "Assistant" });
    await expect(dock).toBeVisible();

    await dock.getByRole("textbox").last().fill(
      "Crée un workflow simple de contrôle santé, lance-le et surveille son run. " +
        "S'il échoue, diagnostique le problème et propose la correction.",
    );
    await dock.getByRole("button", { name: "Send" }).click();

    await expect(
      dock.getByText(/Je vais créer un workflow de contrôle santé/),
    ).toBeVisible();
    await expect(dock.getByText("Create bot bundle demo-healthcheck")).toBeVisible();
    await presentationPause(page);
  });

  await test.step("Confirmer la création du workflow", async () => {
    const dock = page.getByRole("dialog", { name: "Assistant" });
    const created = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        new URL(response.url()).pathname === "/api/v1/bots" &&
        response.ok(),
    );
    await dock.getByRole("button", { name: "Confirm action" }).click();
    await created;
    await expect(dock.getByText("Launch catalog bot demo-healthcheck")).toBeVisible();
    await presentationPause(page);
  });

  let targetRunId = "";
  await test.step("Lancer le run depuis la conversation", async () => {
    const dock = page.getByRole("dialog", { name: "Assistant" });
    const launched = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        new URL(response.url()).pathname === "/api/runs" &&
        response.ok(),
    );
    await dock.getByRole("button", { name: "Confirm action" }).click();
    const response = await launched;
    const body = (await response.json()) as { run_id?: string };
    targetRunId = body.run_id ?? "";
    expect(targetRunId).not.toBe("");

    await expect(dock.getByRole("link", { name: "Open run" })).toHaveAttribute(
      "href",
      `/runs/${targetRunId}`,
    );
    await expect(
      dock.getByText(`Watch run ${targetRunId} in propose mode`),
    ).toBeVisible();
    await presentationPause(page);
  });

  await test.step("Armer la veille durable", async () => {
    const dock = page.getByRole("dialog", { name: "Assistant" });
    const watched = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        new URL(response.url()).pathname ===
          `/api/runs/${targetRunId}/assistant-watches` &&
        response.ok(),
    );
    await dock.getByRole("button", { name: "Confirm action" }).click();
    await watched;
    await expect(
      dock.getByText(
        /Copi is (?:standing by on|working on your request while watching) 1 run tree/,
      ),
    ).toBeVisible();
    await expect(dock.getByText(/La veille est armée/)).toBeVisible();
    await presentationPause(page);
  });

  await test.step("Observer l'échec intentionnel", async () => {
    await expect
      .poll(() => runStatus(request, targetRunId), {
        timeout: WATCH_TIMEOUT_MS,
        message: "the target run should become failed_resumable",
      })
      .toBe("failed_resumable");
    await expect
      .poll(async () => (await mockState(request)).completionRequests, {
        timeout: WATCH_TIMEOUT_MS,
      })
      .toBe(2);
    await presentationPause(page);
  });

  await test.step("Laisser Copi diagnostiquer et proposer la reprise", async () => {
    const dock = page.getByRole("dialog", { name: "Assistant" });
    await expect(
      dock.getByText(`J'ai détecté l'échec intentionnel du run ${targetRunId}.`),
    ).toBeVisible({ timeout: WATCH_TIMEOUT_MS });
    await expect(dock.getByText(`Resume run ${targetRunId}`)).toBeVisible();
    await presentationPause(page);
  });

  await test.step("Confirmer la correction et reprendre le run", async () => {
    const controlUrl = seed().mockOpenAIControlUrl;
    if (!controlUrl) throw new Error("Copi demo mock is not enabled");
    const switched = await request.post(`${controlUrl}/mode`, {
      data: { mode: "succeed" },
    });
    expect(switched.ok()).toBeTruthy();

    const dock = page.getByRole("dialog", { name: "Assistant" });
    const resumed = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        new URL(response.url()).pathname === `/api/runs/${targetRunId}/resume` &&
        response.ok(),
    );
    await dock.getByRole("button", { name: "Confirm action" }).click();
    await resumed;
    await expect(dock.getByText(/La correction est appliquée/)).toBeVisible();
    await presentationPause(page);
  });

  await test.step("Constater la réussite finale sous surveillance", async () => {
    const dock = page.getByRole("dialog", { name: "Assistant" });
    await expect(
      dock.getByText(`Le run ${targetRunId} est terminé avec succès.`),
    ).toBeVisible({ timeout: WATCH_TIMEOUT_MS });
    await expect
      .poll(() => runStatus(request, targetRunId), {
        timeout: WATCH_TIMEOUT_MS,
      })
      .toBe("finished");
    await expect
      .poll(async () => (await mockState(request)).completionRequests, {
        timeout: WATCH_TIMEOUT_MS,
      })
      .toBe(3);
    await presentationPause(page);
  });
});
