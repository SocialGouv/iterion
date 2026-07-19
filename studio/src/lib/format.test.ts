import { describe, expect, it } from "vitest";
import {
  formatDate,
  formatDateTime,
  formatDayHeader,
  formatRelative,
  formatTime,
} from "./format";

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

describe("formatTime", () => {
  it("renders wall-clock time only", () => {
    const out = formatTime("2026-07-18T14:32:05Z");
    expect(out).toMatch(/\d{1,2}:\d{2}:\d{2}/);
    expect(out).not.toContain("2026");
    expect(out).not.toMatch(/Jul/);
  });

  it("returns the em-dash fallback for missing or unparsable input", () => {
    expect(formatTime(undefined)).toBe("—");
    expect(formatTime(null)).toBe("—");
    expect(formatTime("garbage")).toBe("—");
  });
});

describe("formatDayHeader", () => {
  it("renders a short weekday + date without the current year", () => {
    const now = new Date();
    const out = formatDayHeader(now.toISOString());
    expect(out).toMatch(/^[A-Z][a-z]{2}, [A-Z][a-z]{2} \d{1,2}$/);
  });

  it("appends the year for instants outside the current year", () => {
    const out = formatDayHeader("2000-07-18T12:00:00Z");
    expect(out).toContain("2000");
    expect(out).toMatch(/Jul 1[89]/);
  });

  it("returns the em-dash fallback for missing or unparsable input", () => {
    expect(formatDayHeader(undefined)).toBe("—");
    expect(formatDayHeader(null)).toBe("—");
    expect(formatDayHeader("garbage")).toBe("—");
  });
});

describe("formatRelative", () => {
  it("renders past instants with the ago suffix", () => {
    const past = new Date(Date.now() - 5 * 60_000).toISOString();
    expect(formatRelative(past)).toBe("5m ago");
  });

  it("renders future instants with the in prefix", () => {
    const future = new Date(Date.now() + 30 * 86_400_000).toISOString();
    expect(formatRelative(future)).toBe("in 30d");
  });

  it("passes unparsable input through", () => {
    expect(formatRelative("garbage")).toBe("garbage");
  });
});
