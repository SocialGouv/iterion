import { describe, expect, it } from "vitest";

import { parseGoDuration } from "./duration";

describe("parseGoDuration", () => {
  it("parses single units", () => {
    expect(parseGoDuration("500ms")).toBe(500);
    expect(parseGoDuration("90s")).toBe(90_000);
    expect(parseGoDuration("30m")).toBe(1_800_000);
    expect(parseGoDuration("2h")).toBe(7_200_000);
  });

  it("parses combined + fractional units", () => {
    expect(parseGoDuration("1h30m")).toBe(5_400_000);
    expect(parseGoDuration("1.5h")).toBe(5_400_000);
    expect(parseGoDuration("2h45m30s")).toBe(9_930_000);
  });

  it("returns null for empty / nullish / unparseable", () => {
    expect(parseGoDuration(undefined)).toBeNull();
    expect(parseGoDuration(null)).toBeNull();
    expect(parseGoDuration("")).toBeNull();
    expect(parseGoDuration("0")).toBeNull(); // Go "0" (unitless) → treat as no cap
    expect(parseGoDuration("soon")).toBeNull();
    expect(parseGoDuration("30x")).toBeNull(); // stray trailing token rejects
  });

  it("honours a leading sign", () => {
    expect(parseGoDuration("-5m")).toBe(-300_000);
    expect(parseGoDuration("+5m")).toBe(300_000);
  });
});
