import { spawnSync } from "node:child_process";

import { expect, test, type APIRequestContext, type Page, type WebSocketRoute } from "@playwright/test";

import { studio } from "../lib/paths";
import { seed } from "../lib/state";

// studio-ui.run-origin — a run recreated under its ID (A → B) never receives
// what the open view meant for A. Each scenario opens its own run, created by
// the CLI under a fixed ID, never a shared fixture. The view learns A's birth;
// the run is then deleted through the API, its tombstone reaped by
// `iterion runs prune` and the ID recreated by `iterion run --run-id` (B).
//
// The page's run socket is routed (page.routeWebSocket) so the browser cannot
// learn the loss through the socket the server closes on the deletion: after
// the replacement, that socket's error and close frames are withheld and the
// page's side stays open; the view's reads of the run are held too, so no
// poller learns it first. What the view then sends for A meets the server's
// comparison instead:
//   E1 — a rename: its own request answers 409 run_origin_lost, the view
//        shows the French banner, and B keeps its name.
//   E2 — a transport drop: the redial names A's birth, the server answers
//        run_origin_lost on that socket, the view shows the banner and never
//        dials again.
//   E3 — the banner's reload starts the view over on B, with no banner.
// These are regression witnesses of the gate and the banner (they hold on a
// server whose actions are only admitted, not bound); the binding's own
// witnesses are the Go tests of pkg/server and pkg/runview.

const BANNER = "Ce run a été remplacé ou supprimé, ou votre accès a changé.";

// The run the current scenario created (createRun), for its afterEach.
let scenarioRunID: string | null = null;

// iterion runs the binary under test against the seeded store, isolated as
// the server is.
function iterion(args: string[]): string {
  const { cli, workspace } = seed();
  const res = spawnSync(cli.bin, args, { cwd: workspace, env: { ...process.env, ...cli.env }, encoding: "utf8" });
  if (res.status !== 0) {
    throw new Error(`iterion ${args.join(" ")} exited ${res.status}\n${res.stdout}\n${res.stderr}`);
  }
  return res.stdout;
}

// createRun runs the demo bot to completion under runID, named name.
async function createRun(request: APIRequestContext, runID: string, name: string): Promise<string> {
  scenarioRunID = runID;
  const out = iterion(["run", "bots/demo-bot/main.bot", "--store-dir", seed().storeDir, "--run-id", runID, "--json"]);
  const result = JSON.parse(out.slice(out.lastIndexOf("\n{") + 1)) as { run_id: string; status: string };
  expect(result.run_id).toBe(runID);
  expect(result.status).toBe("finished");
  // No expectation rides this request: it names whatever run holds the ID.
  const renamed = await request.post(`/api/runs/${runID}/rename`, { data: { name } });
  expect(renamed.status(), await renamed.text()).toBe(200);
  return birthOf(request, runID);
}

interface StoredRun {
  name: string;
  birth_id: string;
}

async function storedRun(request: APIRequestContext, runID: string): Promise<StoredRun> {
  const res = await request.get(`/api/runs/${runID}`);
  expect(res.status(), await res.text()).toBe(200);
  return ((await res.json()) as { run: StoredRun }).run;
}

async function birthOf(request: APIRequestContext, runID: string): Promise<string> {
  const birth = (await storedRun(request, runID)).birth_id;
  expect(birth, `the birth of ${runID}`).toBeTruthy();
  return birth;
}

// replaceRun deletes the run, reaps its tombstone and recreates the ID: B.
// --keep-last above any run count prunes no run; the tombstone sweep still
// reaps every marker older than --older-than.
async function replaceRun(request: APIRequestContext, runID: string, name: string): Promise<string> {
  const deleted = await request.delete(`/api/runs/${runID}`);
  expect(deleted.status(), await deleted.text()).toBe(204);
  iterion(["runs", "prune", "--store-dir", seed().storeDir, "--older-than", "1ms", "--keep-last", "1000000"]);
  return createRun(request, runID, name);
}

interface SocketTrace {
  url: string;
  page: WebSocketRoute;
  // Frames the server sent, forwarded or not.
  frames: string[];
  serverClosed: { code: number | undefined; reason: string | undefined } | null;
  pageClosed: boolean;
  withhold: boolean;
}

// routeRunSocket forwards every socket the page opens on the run, in both
// directions, and records each one. withholdCurrent() makes the sockets open
// now keep their error and close frames from the page — answering the page's
// pings itself once the server side is gone — while later sockets forward
// everything.
async function routeRunSocket(page: Page, runID: string) {
  const sockets: SocketTrace[] = [];
  const pattern = new RegExp(`/api/ws/runs/${runID}(\\?|$)`);
  await page.routeWebSocket(pattern, (ws) => {
    const trace: SocketTrace = { url: ws.url(), page: ws, frames: [], serverClosed: null, pageClosed: false, withhold: false };
    sockets.push(trace);
    const server = ws.connectToServer();
    ws.onMessage((message) => {
      if (trace.serverClosed === null) {
        server.send(message);
        return;
      }
      const env = JSON.parse(String(message)) as { type?: string; ack_id?: string };
      if (env.type === "ping") ws.send(JSON.stringify({ type: "pong", payload: {}, ack_id: env.ack_id }));
    });
    server.onMessage((message) => {
      trace.frames.push(String(message));
      const env = JSON.parse(String(message)) as { type?: string };
      if (trace.withhold && env.type === "error") return;
      ws.send(message);
    });
    server.onClose((code, reason) => {
      trace.serverClosed = { code, reason };
      if (!trace.withhold) void ws.close({ code, reason });
    });
    ws.onClose((code, reason) => {
      trace.pageClosed = true;
      void server.close({ code, reason });
    });
  });
  return {
    sockets,
    withholdCurrent: () => {
      for (const s of sockets) s.withhold = true;
    },
  };
}

// holdRunReads aborts, while held, every GET the page addresses to the run:
// the view's pollers (the assistant-watch badge reads every 20 s with the
// expected birth) would otherwise learn the replacement from their own 409
// before the scenario's action does. Everything else passes.
async function holdRunReads(page: Page, runID: string) {
  let held = false;
  await page.route(new RegExp(`/api/runs/${runID}(/|\\?|$)`), (route) =>
    held && route.request().method() === "GET" ? route.abort() : route.continue(),
  );
  return {
    hold: () => {
      held = true;
    },
    release: () => {
      held = false;
    },
  };
}

function hasSnapshot(trace: SocketTrace): boolean {
  return trace.frames.some((f) => (JSON.parse(f) as { type?: string }).type === "snapshot");
}

function errorCodes(trace: SocketTrace): string[] {
  return trace.frames
    .map((f) => JSON.parse(f) as { type?: string; payload?: { code?: string } })
    .filter((env) => env.type === "error")
    .map((env) => env.payload?.code ?? "");
}

// openOnA opens the run's view, waits until it shows A and its socket is
// subscribed, then replaces the run by B behind the withheld socket and the
// held reads. It returns A's and B's births, the socket the view held and
// the reads' release.
async function openOnA(page: Page, request: APIRequestContext, runID: string) {
  const birthA = await createRun(request, runID, `${runID}-A`);
  const route = await routeRunSocket(page, runID);
  const reads = await holdRunReads(page, runID);
  await page.goto(`${studio()}/runs/${runID}`);
  await expect(page.getByRole("button", { name: `${runID}-A`, exact: true })).toBeVisible();
  await expect.poll(() => route.sockets.length > 0 && hasSnapshot(route.sockets[0]!)).toBe(true);
  const held = route.sockets[0]!;

  route.withholdCurrent();
  reads.hold();
  const birthB = await replaceRun(request, runID, `${runID}-B`);
  expect(birthB).not.toBe(birthA);
  // The server knew: it closed the view's socket on the deletion. The page
  // did not: that close was withheld, its side is open, no banner shows.
  await expect.poll(() => held.serverClosed?.reason ?? "").toMatch(/^scope_lost: /);
  expect(held.pageClosed).toBe(false);
  await expect(page.getByRole("alert").filter({ hasText: BANNER })).toHaveCount(0);
  return { route, held, reads, birthA, birthB };
}

// renameRefused renames the view's run and requires the mutation's own
// answer: 409 run_origin_lost for the run, sent with A's birth.
async function renameRefused(page: Page, runID: string, birthA: string): Promise<void> {
  await page.getByRole("button", { name: "Rename run" }).click();
  const answered = page.waitForResponse(
    (res) => res.request().method() === "POST" && new URL(res.url()).pathname.endsWith(`/api/runs/${runID}/rename`),
  );
  await page.getByRole("textbox", { name: "Rename run" }).fill(`${runID}-meant-for-A`);
  await page.getByRole("textbox", { name: "Rename run" }).press("Enter");
  const res = await answered;
  expect(res.request().headers()["x-iterion-expected-birth"]).toBe(birthA);
  expect(res.status(), await res.text()).toBe(409);
  const body = (await res.json()) as { error_code?: string; run_id?: string };
  expect(body.error_code).toBe("run_origin_lost");
  expect(body.run_id).toBe(runID);
}

// scenarioRun is the scenario's own run ID: fixed, and distinct per
// repetition (--repeat-each), since the CLI never reuses a live ID.
function scenarioRun(name: string): string {
  return `e2e-origin-${name}-${test.info().repeatEachIndex}`;
}

// The scenario's run is deleted once it ends: the store is the whole
// suite's, and the specs after this one read its run list (one demo-bot run).
test.afterEach(async ({ request }) => {
  const runID = scenarioRunID;
  scenarioRunID = null;
  if (runID === null) return;
  const res = await request.delete(`/api/runs/${runID}`);
  // 404: a scenario that failed between the deletion and the recreation
  // left no run behind.
  expect([204, 404], `delete ${runID}: ${await res.text()}`).toContain(res.status());
});

async function expectBanner(page: Page): Promise<void> {
  const banner = page.getByRole("alert").filter({ hasText: BANNER });
  await expect(banner).toBeVisible();
  await expect(banner.getByRole("button", { name: "Recharger" })).toBeVisible();
}

test("E1: a mutation meant for a replaced run is refused by its own request", async ({ page, request }) => {
  const runID = scenarioRun("e1");
  const { held, birthA, birthB } = await openOnA(page, request, runID);
  expect(held.pageClosed).toBe(false);
  await renameRefused(page, runID, birthA);
  await expectBanner(page);
  const b = await storedRun(request, runID);
  expect(b.birth_id).toBe(birthB);
  expect(b.name).toBe(`${runID}-B`);
});

test("E2: a redial after a transport drop names the learned birth and ends lost", async ({ page, request }) => {
  const runID = scenarioRun("e2");
  const { route, held, birthA } = await openOnA(page, request, runID);
  // A transport drop as the page sees it: not scope_lost, so the hook redials.
  await held.page.close({ code: 4000, reason: "e2e transport drop" });
  await expect.poll(() => route.sockets.length, { timeout: 10_000 }).toBe(2);
  const redial = route.sockets[1]!;
  expect(new URL(redial.url).searchParams.get("expected_birth")).toBe(birthA);
  await expect.poll(() => errorCodes(redial)).toContain("run_origin_lost");
  await expect.poll(() => redial.serverClosed?.reason ?? "").toMatch(/^scope_lost: /);
  expect(hasSnapshot(redial)).toBe(false);
  await expectBanner(page);
  // Lost for good: no further dial.
  await page.waitForTimeout(5_000);
  expect(route.sockets).toHaveLength(2);
});

test("E3: the banner's reload shows the run that holds the ID now", async ({ page, request }) => {
  const runID = scenarioRun("e3");
  const { reads, birthA, birthB } = await openOnA(page, request, runID);
  await renameRefused(page, runID, birthA);
  await expectBanner(page);
  reads.release();
  await page.getByRole("button", { name: "Recharger" }).click();
  await expect(page.getByRole("button", { name: `${runID}-B`, exact: true })).toBeVisible();
  await expect(page.getByRole("alert").filter({ hasText: BANNER })).toHaveCount(0);
  expect((await storedRun(request, runID)).birth_id).toBe(birthB);
});
