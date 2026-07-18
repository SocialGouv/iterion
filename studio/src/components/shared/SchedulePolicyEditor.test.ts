import { describe, expect, it } from "vitest";

import {
  policyFieldsFromValue,
  policyValueFromSchedule,
} from "./SchedulePolicyEditor";

describe("policyValueFromSchedule", () => {
  it("defaults to skip with empty fields", () => {
    expect(policyValueFromSchedule()).toEqual({
      overlap: "skip",
      maxConcurrent: "",
      guard: "",
      guardTimeout: "",
      guardVar: "",
    });
  });

  it("maps an existing schedule's policy into the draft", () => {
    expect(
      policyValueFromSchedule({
        overlap: "allow",
        max_concurrent: 2,
        guard: "true",
        guard_timeout: "10s",
        guard_var: "out",
      }),
    ).toEqual({
      overlap: "allow",
      maxConcurrent: "2",
      guard: "true",
      guardTimeout: "10s",
      guardVar: "out",
    });
  });
});

describe("policyFieldsFromValue", () => {
  it("zeroes max_concurrent unless overlap=allow with a numeric cap", () => {
    const base = policyValueFromSchedule();
    expect(policyFieldsFromValue({ ...base, maxConcurrent: "3" }).max_concurrent).toBe(0);
    expect(
      policyFieldsFromValue({ ...base, overlap: "allow", maxConcurrent: "3" }).max_concurrent,
    ).toBe(3);
    expect(
      policyFieldsFromValue({ ...base, overlap: "allow", maxConcurrent: "abc" }).max_concurrent,
    ).toBe(0);
  });

  it("trims guard fields so a cleared input clears the server value", () => {
    const v = policyValueFromSchedule({ guard: "true", guard_timeout: "10s", guard_var: "x" });
    expect(policyFieldsFromValue({ ...v, guard: "  " })).toMatchObject({
      guard: "",
      guard_timeout: "10s",
      guard_var: "x",
    });
  });
});
