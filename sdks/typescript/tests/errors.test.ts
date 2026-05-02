import { describe, expect, it } from "vitest";

import {
  IterionInvocationError,
  IterionRuntimeError,
  parseRuntimeError,
} from "../src/index.js";

describe("parseRuntimeError", () => {
  it("returns null on empty input", () => {
    expect(parseRuntimeError("")).toBeNull();
  });

  it("returns null when the error preamble is absent", () => {
    expect(parseRuntimeError("nothing structured here")).toBeNull();
  });

  it("parses code + message", () => {
    const err = parseRuntimeError("error [BUDGET_EXCEEDED]: ran out of budget\n");
    expect(err).toBeInstanceOf(IterionRuntimeError);
    expect(err?.code).toBe("BUDGET_EXCEEDED");
    expect(err?.message).toBe("ran out of budget");
  });

  it("parses node and hint when present", () => {
    const stderr =
      "error [LOOP_EXHAUSTED]: too many iterations\n  node: planner\n  hint: bump the loop bound\n";
    const err = parseRuntimeError(stderr);
    expect(err?.code).toBe("LOOP_EXHAUSTED");
    expect(err?.nodeId).toBe("planner");
    expect(err?.hint).toBe("bump the loop bound");
    expect(err?.stderr).toBe(stderr);
  });

  it("tolerates extra surrounding output (logs before the error)", () => {
    const stderr = [
      "info: starting workflow",
      "warn: budget at 90%",
      "error [TIMEOUT]: deadline exceeded",
      "  node: judge",
    ].join("\n");
    const err = parseRuntimeError(stderr);
    expect(err?.code).toBe("TIMEOUT");
    expect(err?.message).toBe("deadline exceeded");
    expect(err?.nodeId).toBe("judge");
  });
});

describe("IterionInvocationError", () => {
  it("preserves the bin/args/exit/stdout/stderr context", () => {
    const err = new IterionInvocationError(
      "boom",
      "iterion",
      ["run", "x.iter"],
      2,
      "out",
      "err",
    );
    expect(err.cmd).toBe("iterion");
    expect(err.args).toEqual(["run", "x.iter"]);
    expect(err.exitCode).toBe(2);
    expect(err.stdout).toBe("out");
    expect(err.stderr).toBe("err");
  });
});
