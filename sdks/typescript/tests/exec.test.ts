import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { execIterion, parseFirstJSON, parseJSON } from "../src/exec.js";
import { IterionInvocationError } from "../src/index.js";

const HERE = dirname(fileURLToPath(import.meta.url));
const FAKE = join(HERE, "fixtures", "fake-iterion.mjs");

describe("execIterion", () => {
  let tmp: string;
  beforeEach(async () => {
    tmp = await mkdtemp(join(tmpdir(), "iterion-sdk-exec-"));
  });
  afterEach(async () => {
    await rm(tmp, { recursive: true, force: true });
  });

  it("captures stdout/stderr and exit code", async () => {
    const r = await execIterion("node", [FAKE, "version"], {
      env: { ...process.env, FAKE_ITERION_SCENARIO: "version" },
    });
    expect(r.exitCode).toBe(0);
    expect(r.stdout.trim()).toBe("v0.2.0-test");
    expect(r.stderr).toBe("");
  });

  it("surfaces non-zero exit codes", async () => {
    const r = await execIterion("node", [FAKE], {
      env: { ...process.env, FAKE_ITERION_EXIT: "3", FAKE_ITERION_STDERR: "boom\n" },
    });
    expect(r.exitCode).toBe(3);
    expect(r.stderr).toBe("boom\n");
  });

  it("invokes onStderrLine for each complete line", async () => {
    const lines: string[] = [];
    await execIterion("node", [FAKE], {
      env: {
        ...process.env,
        FAKE_ITERION_STDERR: "first\nsecond\nthird\n",
      },
      onStderrLine: (l) => lines.push(l),
    });
    expect(lines).toEqual(["first", "second", "third"]);
  });

  it("records the argv passed to the binary", async () => {
    const recordPath = join(tmp, "calls.jsonl");
    await execIterion("node", [FAKE, "--json", "validate", "x.iter"], {
      env: {
        ...process.env,
        FAKE_ITERION_RECORD: recordPath,
        FAKE_ITERION_SCENARIO: "validate-ok",
      },
    });
    const lines = (await readFile(recordPath, "utf8")).trim().split("\n");
    expect(lines).toHaveLength(1);
    const parsed = JSON.parse(lines[0]!);
    expect(parsed.argv).toEqual(["--json", "validate", "x.iter"]);
  });

  it("times out long-running invocations", async () => {
    await expect(
      execIterion("node", [FAKE], {
        env: { ...process.env, FAKE_ITERION_DELAY_MS: "5000" },
        timeoutMs: 100,
      }),
    ).rejects.toBeInstanceOf(IterionInvocationError);
  });

  it("cancels via AbortSignal", async () => {
    const ac = new AbortController();
    const promise = execIterion("node", [FAKE], {
      env: { ...process.env, FAKE_ITERION_DELAY_MS: "5000" },
      signal: ac.signal,
    });
    setTimeout(() => ac.abort(), 50);
    await expect(promise).rejects.toBeInstanceOf(IterionInvocationError);
  });
});

describe("parseJSON", () => {
  const ctx = { bin: "iterion", args: ["x"], stderr: "", exitCode: 0 };

  it("parses valid JSON", () => {
    expect(parseJSON<{ a: number }>('{"a":1}', ctx)).toEqual({ a: 1 });
  });

  it("throws on invalid JSON", () => {
    expect(() => parseJSON("not json", ctx)).toThrow(IterionInvocationError);
  });

  it("throws on empty stdout", () => {
    expect(() => parseJSON("   ", ctx)).toThrow(IterionInvocationError);
  });
});

describe("parseFirstJSON", () => {
  const ctx = { bin: "iterion", args: ["x"], stderr: "", exitCode: 0 };

  it("returns the only object when stdout has a single value", () => {
    expect(parseFirstJSON<{ valid: boolean }>('{"valid":true}\n', ctx)).toEqual({
      valid: true,
    });
  });

  it("returns the FIRST object when stdout has two concatenated envelopes", () => {
    // Mirrors the real iterion `--json validate <bad>` case.
    const stdout =
      JSON.stringify({ file: "bad.iter", valid: false, parse_diagnostics: ["x"] }) +
      "\n" +
      JSON.stringify({ error: "validation failed" }) +
      "\n";
    const got = parseFirstJSON<{ valid: boolean; parse_diagnostics: string[] }>(stdout, ctx);
    expect(got.valid).toBe(false);
    expect(got.parse_diagnostics).toEqual(["x"]);
  });

  it("returns the first array when stdout starts with an array", () => {
    expect(parseFirstJSON<number[]>("[1,2,3]\n{\"trailing\":true}", ctx)).toEqual([1, 2, 3]);
  });

  it("does not stop on `}` inside string literals", () => {
    const stdout = JSON.stringify({ msg: "}}}}", n: 1 }) + "\n" + JSON.stringify({ extra: 1 });
    expect(parseFirstJSON<{ msg: string; n: number }>(stdout, ctx)).toEqual({
      msg: "}}}}",
      n: 1,
    });
  });

  it("throws on empty stdout", () => {
    expect(() => parseFirstJSON("   ", ctx)).toThrow(IterionInvocationError);
  });

  it("throws when no complete JSON value is present", () => {
    expect(() => parseFirstJSON('{"unterminated":', ctx)).toThrow(IterionInvocationError);
  });
});
