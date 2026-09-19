import { describe, expect, it } from "vitest";

import { parseWindow, storedOf } from "./usageCaps";
import type { UsageCapsView } from "@/api/adminSettings";

describe("parseWindow", () => {
  it("treats a blank field as clear-to-env (null, no error)", () => {
    expect(parseWindow("")).toEqual({ value: null });
    expect(parseWindow("   ")).toEqual({ value: null });
  });

  it("accepts a whole number 0–100", () => {
    expect(parseWindow("0")).toEqual({ value: 0 });
    expect(parseWindow("80")).toEqual({ value: 80 });
    expect(parseWindow("100")).toEqual({ value: 100 });
  });

  it("rejects out-of-range, fractional and non-numeric entries", () => {
    expect(parseWindow("-1").error).toBeTruthy();
    expect(parseWindow("101").error).toBeTruthy();
    expect(parseWindow("50.5").error).toBeTruthy();
    expect(parseWindow("abc").error).toBeTruthy();
  });
});

describe("storedOf", () => {
  const base: UsageCapsView = {
    effective: { five_hour_pct: 90, five_hour_mode: "block", week_pct: 90, week_mode: "block" },
    env: { five_hour_pct: 90, week_pct: 90 },
    propagation_bound_seconds: 60,
    source: "env",
  };

  it("returns null when there is no override record", () => {
    expect(storedOf(base, "five_hour_pct")).toBeNull();
    expect(storedOf(undefined, "week_pct")).toBeNull();
  });

  it("reads a stored override when present", () => {
    const view: UsageCapsView = {
      ...base,
      source: "db+env",
      record: { five_hour_pct: 80, updated_at: "2026-09-18T00:00:00Z" },
    };
    expect(storedOf(view, "five_hour_pct")).toBe(80);
    // A field absent from the record is still an inherit (null), not 0.
    expect(storedOf(view, "week_pct")).toBeNull();
  });
});
