import { describe, expect, test } from "vitest";

import type { CodemodeLimits } from "@/api/runs";

import {
  belowMemoryFloor,
  buildCodemodePayload,
  droppableDuration,
  emptyCodemodeFieldValues,
  overCeiling,
  parseDurationSeconds,
  unreadableFields,
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
  memory_floor: 67108864,
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

  test("a sub-1 fraction floors to zero and is DROPPED — no zero travels", () => {
    // The revi gate: flooring AFTER the positivity test sent
    // `tool_calls: 0`, the exact zero this module promises never to send,
    // while the form claimed to override the bot.
    expect(
      buildCodemodePayload("", { ...emptyCodemodeFieldValues, toolCalls: "0.5" }),
    ).toEqual({});
    expect(
      buildCodemodePayload("", {
        ...emptyCodemodeFieldValues,
        toolCalls: "0.5",
        memory: "0.9",
      }).codemode_bounds,
    ).toBeUndefined();
    // …and the form does not claim an override for it either.
    expect(
      buildCodemodePayload("on", { ...emptyCodemodeFieldValues, memory: "0.9" }),
    ).toEqual({ codemode: "on" });
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

  test("a fraction just above the ceiling is NOT flagged — the payload floors it to the ceiling", () => {
    // Adversarial round: comparing the raw float warned where the run
    // receives exactly the ceiling — a divergence no run has.
    expect(
      overCeiling(
        { ...emptyCodemodeFieldValues, memory: String(limits.ceiling.memory! + 0.9) },
        limits,
      ),
    ).toEqual([]);
  });
});

describe("droppableDuration", () => {
  test("reads a zero duration as droppable and a real one as not", () => {
    expect(droppableDuration("0s")).toBe(true);
    expect(droppableDuration("0m")).toBe(true);
    expect(droppableDuration("30s")).toBe(false);
    expect(droppableDuration("soon")).toBe(false);
  });

  test("a duration that parses to zero is dropped — the resolver would read it as unset", () => {
    // The revi gate: "0s" travelled and silently reverted to the bot.
    expect(
      buildCodemodePayload("", { ...emptyCodemodeFieldValues, wall: "0s", cpu: "0m" }),
    ).toEqual({});
  });

  test("an unparsable duration still travels — the server names it in a 400", () => {
    expect(
      buildCodemodePayload("", { ...emptyCodemodeFieldValues, wall: "soon" }),
    ).toEqual({ codemode_bounds: { wall: "soon" } });
  });

  test("a zero duration is marked unreadable so the field says so", () => {
    expect(
      unreadableFields({ ...emptyCodemodeFieldValues, wall: "0s", cpu: "2m" }),
    ).toEqual(["wall"]);
  });
});

describe("belowMemoryFloor", () => {
  test("names the floor for a memory bound the child cannot start at", () => {
    expect(
      belowMemoryFloor({ ...emptyCodemodeFieldValues, memory: "1000" }, limits),
    ).toBe(67108864);
  });

  test("a memory bound at or above the floor is silent", () => {
    expect(
      belowMemoryFloor({ ...emptyCodemodeFieldValues, memory: "67108864" }, limits),
    ).toBeUndefined();
    expect(
      belowMemoryFloor({ ...emptyCodemodeFieldValues, memory: "268435456" }, limits),
    ).toBeUndefined();
  });

  test("an empty or unreadable field is silence — nothing will be sent", () => {
    expect(belowMemoryFloor(emptyCodemodeFieldValues, limits)).toBeUndefined();
    expect(
      belowMemoryFloor({ ...emptyCodemodeFieldValues, memory: "1 Mio" }, limits),
    ).toBeUndefined();
  });
});

describe("unreadableFields", () => {
  test("names the numeric fields holding a value the payload drops", () => {
    expect(
      unreadableFields({
        ...emptyCodemodeFieldValues,
        toolCalls: "0",
        memory: "-5",
        scriptBytes: "1,000",
        resultBytes: "2048",
      }),
    ).toEqual(["tool_calls", "memory", "script_bytes"]);
  });

  test("an unparsable duration is left to the server — it is not a numeric field", () => {
    expect(
      unreadableFields({ ...emptyCodemodeFieldValues, wall: "soon", cpu: "5m" }),
    ).toEqual([]);
  });
});
