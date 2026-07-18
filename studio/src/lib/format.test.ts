import { describe, expect, it } from "vitest";
import { formatDate, formatDateTime } from "./format";

// Locale is pinned to en-US inside the helpers, but the rendered value
// still depends on the host timezone, so assertions stay shape-loose
// (year present, en-US month abbreviation, time marker) rather than
// pinning an exact instant.

describe("formatDateTime", () => {
  it("renders an absolute en-US date + time", () => {
    const out = formatDateTime("2026-07-18T14:32:00Z");
    expect(out).toContain("2026");
    expect(out).toMatch(/Jul 1[89]/); // day may shift with host TZ
    expect(out).toMatch(/\d{1,2}:\d{2}/);
  });

  it("returns the em-dash fallback for missing input", () => {
    expect(formatDateTime(undefined)).toBe("—");
    expect(formatDateTime(null)).toBe("—");
    expect(formatDateTime("")).toBe("—");
  });

  it("returns the em-dash fallback for unparsable input", () => {
    expect(formatDateTime("not-a-date")).toBe("—");
  });
});

describe("formatDate", () => {
  it("renders an absolute en-US date without a time", () => {
    const out = formatDate("2026-07-18T14:32:00Z");
    expect(out).toContain("2026");
    expect(out).toMatch(/Jul 1[89]/);
    expect(out).not.toMatch(/\d{1,2}:\d{2}/);
  });

  it("returns the em-dash fallback for missing input", () => {
    expect(formatDate(undefined)).toBe("—");
    expect(formatDate(null)).toBe("—");
    expect(formatDate("")).toBe("—");
  });

  it("returns the em-dash fallback for unparsable input", () => {
    expect(formatDate("garbage")).toBe("—");
  });
});
