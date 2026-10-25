import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { closeSync, mkdtempSync, openSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const guard = fileURLToPath(new URL("./guard.mjs", import.meta.url));

for (const ci of [false, true]) {
  test(`missing Chromium ${ci ? "fails CI with an annotation" : "names the local skip"}`, (t) => {
    const dir = mkdtempSync(path.join(tmpdir(), "studio-e2e-guard-"));
    t.after(() => rmSync(dir, { recursive: true, force: true }));
    const summary = path.join(dir, "summary.md");
    const log = path.join(dir, "stderr.log");
    const stderr = openSync(log, "w");
    t.after(() => closeSync(stderr));
    const result = spawnSync(process.execPath, [guard], {
      stdio: ["ignore", "ignore", stderr],
      env: {
        ...process.env,
        // The guard is a CLI, not a child test-runner worker. Inheriting this
        // would serialize its console output as test-runner IPC instead.
        NODE_TEST_CONTEXT: undefined,
        CI: ci ? "true" : "",
        GITHUB_ACTIONS: ci ? "true" : "",
        GITHUB_STEP_SUMMARY: summary,
        PLAYWRIGHT_BROWSERS_PATH: path.join(dir, "no-browsers"),
      },
    });
    assert.ifError(result.error);
    const output = readFileSync(log, "utf8");
    assert.equal(result.status, ci ? 1 : 77, output);
    assert.match(output, /no Playwright Chromium browser installed/);
    assert.match(output, /the Studio UI e2e suite did not run/);
    assert.match(output, /devbox run -- task test:e2e:ui:install/);
    assert.match(output, ci ? /::error title=Studio UI e2e did not run::/ : /SKIPPED:/);
    assert.match(readFileSync(summary, "utf8"), /the Studio UI e2e suite did not run/);
  });
}
