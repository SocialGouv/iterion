// Boots the studio-UI e2e target: a throwaway workspace + store seeded
// with REAL artifacts (runs produced by the engine, a board card created
// through the CLI), then `iterion studio` serving the embedded SPA on it.
//
// Playwright's `webServer` runs this and waits on /api/server/info. Every
// path is deterministic and rebuilt from scratch on each run, so the specs
// can assert exact names, ids and counts.
//
// Isolation: ITERION_HOME + HOME point at the throwaway tree and
// ITERION_SECRETS_KEY pins the sealer, so the suite never reads or writes
// the operator's own ~/.iterion (runs, secrets, keychain).

import { spawn, spawnSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { createMockOpenAI } from "./mock-openai.mjs";

const here = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(here, "..", "..");
const port = Number(process.env.ITERION_UI_PORT ?? 4899);
const origin = `http://127.0.0.1:${port}`;
const copiDemoEnabled = process.env.ITERION_COPI_DEMO === "1";
const mockOpenAIPort = Number(process.env.ITERION_E2E_OPENAI_PORT ?? port + 1);
const mockOpenAIOrigin = `http://127.0.0.1:${mockOpenAIPort}`;

// Workspace lives under studio/e2e/.tmp so a failed run is inspectable.
const ws = path.join(here, ".tmp", "workspace");
const storeDir = path.join(ws, ".iterion");
const home = path.join(ws, "home");

function fail(msg) {
  console.error(`[studio-e2e] ${msg}`);
  process.exit(1);
}

// --- the binary under test ------------------------------------------------

const bin = process.env.ITERION_BIN || path.join(repoRoot, "iterion");
if (!fs.existsSync(bin)) {
  fail(
    `iterion binary not found at ${bin}. Build it first (\`task build\`) ` +
      `or point ITERION_BIN at one. The suite drives the REAL server, and ` +
      `the SPA it serves is the one embedded at build time.`,
  );
}

// --- a clean workspace ----------------------------------------------------

fs.rmSync(ws, { recursive: true, force: true });
fs.mkdirSync(storeDir, { recursive: true });
fs.mkdirSync(home, { recursive: true });
fs.cpSync(path.join(here, "fixtures", "bots"), path.join(ws, "bots"), {
  recursive: true,
});
// Keep the established suite's catalog and backend detection unchanged. The
// deterministic Copi override is enabled only by the focused demo command.
if (!copiDemoEnabled) {
  fs.rmSync(path.join(ws, "bots", "copilot"), { recursive: true, force: true });
}

// The preview fixture's URL must point at this run's own origin so the
// Browser pane's iframe stays on the loopback test server.
const previewBot = path.join(ws, "bots", "preview-bot", "main.bot");
const previewUrl = `${origin}/api/server/info`;
fs.writeFileSync(
  previewBot,
  fs.readFileSync(previewBot, "utf8").replaceAll("__PREVIEW_URL__", previewUrl),
);

// A fixed 32-byte key: the local secret store is sealed with it instead of
// the operator's keychain entry.
const secretsKey = Buffer.alloc(32, 7).toString("base64");

const childEnv = {
  ...process.env,
  HOME: home,
  ITERION_HOME: home,
  ITERION_SECRETS_KEY: secretsKey,
  // The fixtures declare `sandbox: none`, but pin the machine default too
  // so no container runtime is ever probed on a CI box.
  ITERION_SANDBOX_DEFAULT: "none",
  // Keep the seeded runs' backend resolution offline: nothing in the
  // fixtures calls an LLM, and no host credential must leak into a run.
  ITERION_DEFAULT_BACKEND: "claw",
  ...(copiDemoEnabled
    ? {
        // The demo's generated bot exercises the real Claw/OpenAI adapter
        // against a strict loopback fake. Never expose an ambient operator
        // credential to the throwaway server.
        OPENAI_API_KEY: "iterion-e2e-local-only",
        OPENAI_BASE_URL: mockOpenAIOrigin,
        ITERION_OPENAI_USE_OAUTH: "0",
        // The Copi fixture resolves the new run created by run.launch. Keep
        // that test-only read explicit instead of relying on cwd.
        ITERION_E2E_STORE_DIR: storeDir,
      }
    : {}),
};

function run(args, { json = false } = {}) {
  const res = spawnSync(bin, args, {
    cwd: ws,
    env: childEnv,
    encoding: "utf8",
  });
  if (res.status !== 0) {
    fail(
      `\`iterion ${args.join(" ")}\` exited ${res.status}\n${res.stdout}\n${res.stderr}`,
    );
  }
  if (!json) return res.stdout;
  // `--json` prints the machine payload as the trailing JSON object.
  const start = res.stdout.lastIndexOf("\n{");
  const blob = start === -1 ? res.stdout : res.stdout.slice(start + 1);
  try {
    return JSON.parse(blob);
  } catch (e) {
    fail(`could not parse JSON from \`iterion ${args.join(" ")}\`: ${e}\n${res.stdout}`);
  }
}

// --- seed: real runs, a real board card -----------------------------------

const store = ["--store-dir", storeDir];

const fixtureRun = run(
  ["run", "bots/demo-bot/main.bot", ...store, "--json"],
  { json: true },
);
if (fixtureRun.status !== "finished") {
  fail(`seed run did not finish: ${JSON.stringify(fixtureRun)}`);
}

const previewRun = run(
  ["run", "bots/preview-bot/main.bot", ...store, "--json"],
  { json: true },
);
if (previewRun.status !== "finished") {
  fail(`preview seed run did not finish: ${JSON.stringify(previewRun)}`);
}

const issue = run(
  [
    "issue",
    "create",
    ...store,
    "--title",
    "Fixture card in inbox",
    "--body",
    "seeded by the studio UI e2e suite",
    "--label",
    "ui-e2e",
    "--json",
  ],
  { json: true },
);

// Start the strict fake only after the synchronous seed commands have
// completed. It still comes up before Studio, so no model request can race
// past it. The adjacent fixed port makes collisions fail loudly.
const mockOpenAI = copiDemoEnabled ? createMockOpenAI() : null;
if (mockOpenAI) {
  let listeningMockOrigin;
  try {
    listeningMockOrigin = await mockOpenAI.listen({ port: mockOpenAIPort });
  } catch (error) {
    fail(`could not start mock OpenAI at ${mockOpenAIOrigin}: ${error}`);
  }
  if (listeningMockOrigin !== mockOpenAIOrigin) {
    fail(
      `mock OpenAI listened at ${listeningMockOrigin}, expected ${mockOpenAIOrigin}`,
    );
  }
}

fs.writeFileSync(
  path.join(ws, "state.json"),
  JSON.stringify(
    {
      origin,
      workspace: ws,
      storeDir,
      fixtureRunId: fixtureRun.run_id,
      previewRunId: previewRun.run_id,
      previewUrl,
      mockOpenAIControlUrl: mockOpenAI
        ? `${mockOpenAIOrigin}/__control`
        : null,
      issueId: issue.id,
      // Mirrors the --max-concurrent-pipelines the studio boots with; the
      // pipelines spec asserts the UI renders exactly this cap.
      maxConcurrentPipelines: 2,
    },
    null,
    2,
  ),
);

// --- the server under test ------------------------------------------------

const studio = spawn(
  bin,
  [
    "studio",
    "--port",
    String(port),
    "--no-browser",
    "--dir",
    ws,
    ...store,
    "--bots-path",
    path.join(ws, "bots"),
    "--max-concurrent-pipelines",
    "2",
  ],
  { cwd: ws, env: childEnv, stdio: "inherit" },
);

const stop = () => {
  if (!studio.killed) studio.kill("SIGTERM");
};
process.on("SIGTERM", stop);
process.on("SIGINT", stop);
studio.on("error", async (error) => {
  console.error(`[studio-e2e] could not start studio: ${error}`);
  await mockOpenAI?.close();
  process.exitCode = 1;
});
studio.on("exit", async (code) => {
  await mockOpenAI?.close();
  process.exitCode = code ?? 0;
});
