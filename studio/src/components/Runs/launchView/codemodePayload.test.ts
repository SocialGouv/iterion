import { describe, expect, test } from "vitest";

import type { CodemodeLimits } from "@/api/runs";

import {
  buildCodemodePayload,
  emptyCodemodeFieldValues,
  overCeiling,
  parseDurationSeconds,
} from "./codemodePayload";

const limits: CodemodeLimits = {
  default: {
    tool_calls: 64,
    wall: "30s",
    cpu: "10s",
    memory: 268435456,
    script_bytes: 262144,
    result_bytes: 1048576,
    tool_result_bytes: 65536,
  },
  ceiling: {
    tool_calls: 4096,
    wall: "10m0s",
    cpu: "5m0s",
    memory: 2147483648,
    script_bytes: 4194304,
    result_bytes: 8388608,
    tool_result_bytes: 1048576,
  },
};

describe("buildCodemodePayload", () => {
  test("an untouched form sends nothing — the run inherits the bot", () => {
    expect(buildCodemodePayload("", emptyCodemodeFieldValues)).toEqual({});
  });

  test("the mode travels alone when no bound was set", () => {
    expect(buildCodemodePayload("off", emptyCodemodeFieldValues)).toEqual({
      codemode: "off",
    });
  });

  test("every set bound travels in the DSL orthography", () => {
    const payload = buildCodemodePayload("on", {
      toolCalls: "256",
      wall: "90s",
      cpu: "2m",
      memory: "1073741824",
      scriptBytes: "524288",
      resultBytes: "2097152",
      toolResultBytes: "131072",
    });
    expect(payload).toEqual({
      codemode: "on",
      codemode_bounds: {
        tool_calls: 256,
        wall: "90s",
        cpu: "2m",
        memory: 1073741824,
        script_bytes: 524288,
        result_bytes: 2097152,
        tool_result_bytes: 131072,
      },
    });
  });

  test("unset fields stay out of the object — a zero is nobody-chose, a negative is refused", () => {
    const payload = buildCodemodePayload("", {
      ...emptyCodemodeFieldValues,
      toolCalls: "0",
      memory: "-5",
      wall: "abc",
    });
    // toolCalls 0 and memory -5 are inherit; wall is passed through verbatim
    // because the SERVER owns the duration parse and names a bad value.
    expect(payload).toEqual({ codemode_bounds: { wall: "abc" } });
  });

  test("a fractional count is floored rather than sent as a float", () => {
    const payload = buildCodemodePayload("", {
      ...emptyCodemodeFieldValues,
      toolCalls: "12.9",
    });
    expect(payload.codemode_bounds?.tool_calls).toBe(12);
  });
});

describe("parseDurationSeconds", () => {
  test.each([
    ["30s", 30],
    ["5m", 300],
    ["1h", 3600],
    ["1h30m", 5400],
    ["500ms", 0.5],
    ["2m30s", 150],
  ])("%s → %d s", (raw, want) => {
    expect(parseDurationSeconds(raw)).toBe(want);
  });

  test.each(["", "abc", "5m30", "30 s", "-5s"])("%s is not a duration", (raw) => {
    expect(parseDurationSeconds(raw)).toBeUndefined();
  });
});

describe("overCeiling", () => {
  test("names exactly the fields above the platform ceiling", () => {
    const over = overCeiling(
      {
        ...emptyCodemodeFieldValues,
        toolCalls: "5000", // above 4096
        wall: "11m", // above 10m
        cpu: "5m", // AT the ceiling — not over
        memory: "2147483648", // AT the ceiling
        resultBytes: "8388609", // one byte over
      },
      limits,
    );
    expect(over).toEqual(["tool_calls", "wall", "result_bytes"]);
  });

  test("an unparsable duration is not flagged — the server names it", () => {
    expect(
      overCeiling({ ...emptyCodemodeFieldValues, wall: "soon" }, limits),
    ).toEqual([]);
  });

  test("no limits (an older server) means no verdict, never a guess", () => {
    expect(
      overCeiling({ ...emptyCodemodeFieldValues, toolCalls: "999999" }, undefined),
    ).toEqual([]);
  });
});
