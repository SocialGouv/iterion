import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import {
  IterionClient,
  IterionInvocationError,
  IterionRunPausedSignal,
  IterionRuntimeError,
} from "../src/index.js";

const HERE = dirname(fileURLToPath(import.meta.url));
const FAKE = join(HERE, "fixtures", "fake-iterion.mjs");

/**
 * Build a tiny shell wrapper that re-execs `node fake-iterion.mjs`
 * so the IterionClient can spawn it as a single binary path. This
 * keeps the client code identical to production and only the test
 * fixture moves.
 */
async function makeFakeBin(dir: string): Promise<string> {
  const path = join(dir, "iterion");
  const { writeFile, chmod } = await import("node:fs/promises");
  await writeFile(path, `#!/bin/sh\nexec node "${FAKE}" "$@"\n`);
  await chmod(path, 0o755);
  return path;
}

interface RecordedCall {
  argv: string[];
  env: Record<string, string>;
}

async function readCalls(path: string): Promise<RecordedCall[]> {
  try {
    const raw = await readFile(path, "utf8");
    return raw
      .trim()
      .split("\n")
      .filter(Boolean)
      .map((l) => JSON.parse(l) as RecordedCall);
  } catch {
    return [];
  }
}

describe("IterionClient", () => {
  let tmp: string;
  let fakeBin: string;
  let recordPath: string;

  beforeEach(async () => {
    tmp = await mkdtemp(join(tmpdir(), "iterion-sdk-client-"));
    fakeBin = await makeFakeBin(tmp);
    recordPath = join(tmp, "calls.jsonl");
  });

  afterEach(async () => {
    await rm(tmp, { recursive: true, force: true });
  });

  function client(extraEnv: Record<string, string> = {}): IterionClient {
    return new IterionClient({
      binPath: fakeBin,
      env: {
        FAKE_ITERION_RECORD: recordPath,
        ...extraEnv,
      },
    });
  }

  // ---- version -----------------------------------------------------------

  it("version() returns the stdout string", async () => {
    const got = await client({ FAKE_ITERION_SCENARIO: "version" }).version();
    expect(got).toEqual({ version: "v0.2.0-test" });
  });

  // ---- validate ----------------------------------------------------------

  it("validate() parses the structured envelope on success", async () => {
    const got = await client({ FAKE_ITERION_SCENARIO: "validate-ok" }).validate("demo.iter");
    expect(got.valid).toBe(true);
    expect(got.workflow_name).toBe("demo");
    expect(got.file).toBe("demo.iter");
    expect(got.node_count).toBe(3);
    expect(got.edge_count).toBe(2);
    const calls = await readCalls(recordPath);
    expect(calls[0]?.argv).toEqual(["--json", "validate", "demo.iter"]);
  });

  it("validate() surfaces diagnostics even when the CLI double-writes JSON on failure", async () => {
    // The real CLI writes the structured ValidateResult envelope and is
    // followed by `{"error":"validation failed"}` on stdout. The SDK must
    // return the first envelope so consumers can see parse_diagnostics.
    const got = await client({ FAKE_ITERION_SCENARIO: "validate-fail" }).validate("bad.iter");
    expect(got.valid).toBe(false);
    expect(got.file).toBe("bad.iter");
    expect(got.parse_diagnostics).toEqual([
      "bad.iter:1:14: error [E002]: expected :, got Newline",
      "bad.iter:2:1: error [E002]: expected INDENT, got EOF",
    ]);
  });

  it("validate() throws on pre-validation error envelopes", async () => {
    await expect(
      client({ FAKE_ITERION_SCENARIO: "validate-error-envelope" }).validate(
        "/tmp/definitely-no-such-file.iter",
      ),
    ).rejects.toMatchObject({
      name: "IterionInvocationError",
      exitCode: 1,
      stdout: expect.stringContaining("cannot read file"),
      args: ["--json", "validate", "/tmp/definitely-no-such-file.iter"],
    });
    await expect(
      client({ FAKE_ITERION_SCENARIO: "validate-error-envelope" }).validate(
        "/tmp/definitely-no-such-file.iter",
      ),
    ).rejects.toThrow(/iterion validate exited with code 1/);
  });

  // ---- run ---------------------------------------------------------------

  it("run() returns the parsed result on finished", async () => {
    const result = await client({ FAKE_ITERION_SCENARIO: "run-finished" }).run(
      "demo.iter",
      { vars: { repo: "x", branch: "main" }, logLevel: "info" },
    );
    expect(result.status).toBe("finished");
    expect(result.run_id).toBe("run_1");

    const calls = await readCalls(recordPath);
    const argv = calls[0]!.argv;
    expect(argv[0]).toBe("--json");
    expect(argv[1]).toBe("run");
    expect(argv[2]).toBe("demo.iter");
    expect(argv).toContain("--var");
    expect(argv).toContain("repo=x");
    expect(argv).toContain("branch=main");
    expect(argv).toContain("--log-level");
    expect(argv).toContain("info");
    expect(argv).toContain("--no-interactive");
  });

  it("run() returns paused result without throwing", async () => {
    const result = await client({ FAKE_ITERION_SCENARIO: "run-paused" }).run("demo.iter");
    expect(result.status).toBe("paused_waiting_human");
    if (result.status === "paused_waiting_human") {
      expect(result.interaction_id).toBe("run_1_review");
      expect(result.questions).toEqual({ summary: "is this ok?" });
    }
  });

  it("run() throws IterionRuntimeError mapped from stderr on failure", async () => {
    // The run-failed fixture now mirrors the real CLI's double-write
    // (structured `{status:"failed", …}` envelope followed by the bare
    // `{"error":"…"}` envelope from cmd/iterion/main.go's outer error
    // handler). Strict JSON.parse on the full stdout would throw, losing
    // the typed mapping; this test pins the parseFirstJSON contract that
    // makes the structured status survive.
    await expect(
      client({ FAKE_ITERION_SCENARIO: "run-failed" }).run("demo.iter"),
    ).rejects.toMatchObject({
      name: "IterionRuntimeError",
      code: "BUDGET_EXCEEDED",
      nodeId: "planner",
    });
  });

  it("run() falls back to EXECUTION_FAILED with the structured error message when stderr has no preamble", async () => {
    // In --json mode the CLI does NOT emit the `error [CODE]:` preamble
    // (cmd/iterion/main.go writes the JSON envelope instead of calling
    // PrintError). The SDK must therefore use the structured envelope's
    // `error` field as the message rather than treating the failure as
    // a JSON-parse failure or returning a header-less RunResult.
    await expect(
      client({
        FAKE_ITERION_SCENARIO: "run-failed",
        // Drop the stderr preamble so parseRuntimeError(stderr) returns
        // null and we exercise the EXECUTION_FAILED fallback path.
        FAKE_ITERION_STDERR: "",
      }).run("demo.iter"),
    ).rejects.toMatchObject({
      name: "IterionRuntimeError",
      code: "EXECUTION_FAILED",
      message: "budget cap",
    });
  });

  it("run() returns result on cancelled by default (double-write tolerated)", async () => {
    // The run-cancelled fixture also double-writes (structured envelope
    // + bare `{error:"run cancelled"}`). Default behaviour for cancelled
    // is to return the typed result rather than throw.
    const result = await client({ FAKE_ITERION_SCENARIO: "run-cancelled" }).run("demo.iter");
    expect(result.status).toBe("cancelled");
    expect(result.run_id).toBe("run_1");
  });

  it("run() throws on pre-engine error envelope (no status field)", async () => {
    // When the CLI fails BEFORE assigning a status (parse error, missing
    // file, …) it emits only the global `{error: "…"}` envelope. The SDK
    // must surface this as an exception rather than silently returning a
    // header-less RunResult whose `run_id` and `status` are undefined.
    await expect(
      client({ FAKE_ITERION_SCENARIO: "run-error-envelope" }).run("bad.iter"),
    ).rejects.toMatchObject({
      name: "IterionInvocationError",
      exitCode: 1,
    });
    await expect(
      client({ FAKE_ITERION_SCENARIO: "run-error-envelope" }).run("bad.iter"),
    ).rejects.toThrow(/parse error/);
  });

  it("run() throwOn=['paused_waiting_human'] raises IterionRunPausedSignal", async () => {
    await expect(
      client({ FAKE_ITERION_SCENARIO: "run-paused" }).run("demo.iter", {
        throwOn: ["paused_waiting_human"],
      }),
    ).rejects.toBeInstanceOf(IterionRunPausedSignal);
  });

  it("run() throwOn=['cancelled'] raises a runtime error", async () => {
    await expect(
      client({ FAKE_ITERION_SCENARIO: "run-cancelled" }).run("demo.iter", {
        throwOn: ["cancelled"],
      }),
    ).rejects.toBeInstanceOf(IterionRuntimeError);
  });

  it("run() formats numeric timeout as milliseconds", async () => {
    await client({ FAKE_ITERION_SCENARIO: "run-finished" }).run("demo.iter", {
      timeout: 30_000,
    });
    const calls = await readCalls(recordPath);
    const argv = calls[0]!.argv;
    const idx = argv.indexOf("--timeout");
    expect(idx).toBeGreaterThan(-1);
    expect(argv[idx + 1]).toBe("30000ms");
  });

  it("run() passes through string timeout verbatim (Go duration)", async () => {
    await client({ FAKE_ITERION_SCENARIO: "run-finished" }).run("demo.iter", {
      timeout: "5m",
    });
    const calls = await readCalls(recordPath);
    const argv = calls[0]!.argv;
    expect(argv[argv.indexOf("--timeout") + 1]).toBe("5m");
  });

  it("run() applies storeDir from the client when not overridden", async () => {
    await new IterionClient({
      binPath: fakeBin,
      storeDir: "/tmp/iterion-default",
      env: {
        FAKE_ITERION_RECORD: recordPath,
        FAKE_ITERION_SCENARIO: "run-finished",
      },
    }).run("demo.iter");
    const calls = await readCalls(recordPath);
    const argv = calls[0]!.argv;
    expect(argv[argv.indexOf("--store-dir") + 1]).toBe("/tmp/iterion-default");
  });

  // ---- resume ------------------------------------------------------------

  it("resume() builds the right argv for string answers", async () => {
    const got = await client({ FAKE_ITERION_SCENARIO: "resume-finished" }).resume({
      runId: "run_1",
      file: "demo.iter",
      answers: { approve: "yes", reason: "looks good" },
      force: true,
    });
    expect(got.status).toBe("finished");
    const calls = await readCalls(recordPath);
    const argv = calls[0]!.argv;
    expect(argv[0]).toBe("--json");
    expect(argv[1]).toBe("resume");
    expect(argv).toContain("--run-id");
    expect(argv).toContain("run_1");
    expect(argv).toContain("--file");
    expect(argv).toContain("demo.iter");
    expect(argv).toContain("--force");
    expect(argv.filter((a) => a === "--answer").length).toBe(2);
    expect(argv).toContain("approve=yes");
    expect(argv).toContain("reason=looks good");
    // No --answers-file (all answers fit the string flag).
    expect(argv).not.toContain("--answers-file");
  });

  it("resume() materialises non-string answers into a temp --answers-file", async () => {
    await client({ FAKE_ITERION_SCENARIO: "resume-finished" }).resume({
      runId: "run_1",
      file: "demo.iter",
      answers: { approve: true, score: 0.9, note: "fine" },
    });
    const calls = await readCalls(recordPath);
    const argv = calls[0]!.argv;
    const fileIdx = argv.indexOf("--answers-file");
    expect(fileIdx).toBeGreaterThan(-1);
    // Note: by the time we read this back the temp file has been cleaned up,
    // so we just assert the argv shape and the string answer still flowed
    // through --answer.
    expect(argv).toContain("--answer");
    expect(argv).toContain("note=fine");
  });

  it("resume() requires runId/file (compile-time, not asserted at runtime here)", async () => {
    // No-op — guarded by TS types. Included to document the contract.
    expect(true).toBe(true);
  });

  it("resume() throws IterionRuntimeError on failed status (double-write tolerated)", async () => {
    // pkg/cli/resume.go:182-191 emits the structured envelope and
    // returns the engine error, which causes cmd/iterion/main.go to
    // append a second bare `{"error":"…"}` envelope. Verify the SDK
    // still surfaces the typed runtime error.
    await expect(
      client({ FAKE_ITERION_SCENARIO: "resume-failed" }).resume({
        runId: "run_1",
        file: "demo.iter",
      }),
    ).rejects.toMatchObject({
      name: "IterionRuntimeError",
      code: "EXECUTION_FAILED",
      nodeId: "planner",
    });
  });

  it("resume() throws on pre-engine error envelope (no status field)", async () => {
    await expect(
      client({ FAKE_ITERION_SCENARIO: "resume-error-envelope" }).resume({
        runId: "non_existent",
        file: "demo.iter",
      }),
    ).rejects.toMatchObject({
      name: "IterionInvocationError",
      exitCode: 1,
    });
  });

  // ---- inspect / report / diagram ----------------------------------------

  it("inspect() returns array when no runId is given", async () => {
    const got = await client({ FAKE_ITERION_SCENARIO: "inspect-list" }).inspect();
    expect(got).toEqual([]);
  });

  it("inspect() returns single result when runId is given", async () => {
    const got = await client({ FAKE_ITERION_SCENARIO: "inspect-single" }).inspect({
      runId: "run_1",
      events: true,
    });
    expect(Array.isArray(got)).toBe(false);
    if (!Array.isArray(got)) {
      expect(got.run.id).toBe("run_1");
    }
    const calls = await readCalls(recordPath);
    expect(calls[0]?.argv).toEqual([
      "--json",
      "inspect",
      "--run-id",
      "run_1",
      "--events",
    ]);
  });

  it("diagram() forwards --view", async () => {
    await client({ FAKE_ITERION_SCENARIO: "diagram" }).diagram("demo.iter", {
      view: "detailed",
    });
    const calls = await readCalls(recordPath);
    expect(calls[0]?.argv).toEqual(["--json", "diagram", "demo.iter", "--view", "detailed"]);
  });

  it("report() requires --run-id", async () => {
    await client({ FAKE_ITERION_SCENARIO: "report" }).report({ runId: "run_1" });
    const calls = await readCalls(recordPath);
    expect(calls[0]?.argv).toEqual(["--json", "report", "--run-id", "run_1"]);
  });

  // ---- abort -------------------------------------------------------------

  it("aborts in-flight runs via signal", async () => {
    const ac = new AbortController();
    const c = new IterionClient({
      binPath: fakeBin,
      env: { FAKE_ITERION_DELAY_MS: "5000", FAKE_ITERION_SCENARIO: "run-finished" },
      signal: ac.signal,
    });
    const p = c.run("demo.iter");
    setTimeout(() => ac.abort(), 50);
    await expect(p).rejects.toBeInstanceOf(IterionInvocationError);
  });
});
