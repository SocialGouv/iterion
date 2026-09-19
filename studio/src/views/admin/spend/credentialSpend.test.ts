import { describe, expect, it } from "vitest";

import {
  buildQuery,
  formatTokens,
  formatUSD,
  isValidMonth,
  sameQuery,
} from "./credentialSpend";

describe("isValidMonth", () => {
  it("accepts blank (current month) and well-formed YYYY-MM", () => {
    expect(isValidMonth("")).toBe(true);
    expect(isValidMonth("2026-09")).toBe(true);
    expect(isValidMonth("2026-01")).toBe(true);
    expect(isValidMonth("2026-12")).toBe(true);
  });
  it("rejects malformed months", () => {
    expect(isValidMonth("2026-13")).toBe(false);
    expect(isValidMonth("2026-00")).toBe(false);
    expect(isValidMonth("26-09")).toBe(false);
    expect(isValidMonth("2026/09")).toBe(false);
  });
});

describe("buildQuery — fingerprint and repo are mutually exclusive", () => {
  it("defaults to the tier when neither fingerprint nor repo is set", () => {
    expect(buildQuery({ tier: "platform", month: "", fingerprint: "", repo: "" })).toEqual({
      tier: "platform",
    });
  });
  it("sends fingerprint (and drops repo + tier) when a fingerprint is given", () => {
    expect(
      buildQuery({ tier: "org", month: "2026-08", fingerprint: "fp1", repo: "acme/app" }),
    ).toEqual({ month: "2026-08", fingerprint: "fp1" });
  });
  it("sends repo (and drops tier) when only a repo is given", () => {
    expect(buildQuery({ tier: "org", month: "", fingerprint: "", repo: "acme/app" })).toEqual({
      repo: "acme/app",
    });
  });
  it("trims and treats whitespace-only as empty", () => {
    expect(buildQuery({ tier: "team", month: "  ", fingerprint: "  ", repo: "  " })).toEqual({
      tier: "team",
    });
  });
});

// Apply must be able to RETRY a failed fetch, and it can only tell that it is
// a retry (rather than a new question) by comparing the built queries — an
// unchanged one keys the same react-query entry, which re-runs on nothing.
describe("sameQuery", () => {
  const form = { tier: "org" as const, month: "2026-08", fingerprint: "", repo: "" };
  it("is true for two builds of the same filter set", () => {
    expect(sameQuery(buildQuery(form), buildQuery({ ...form }))).toBe(true);
  });
  it("is true across the whitespace buildQuery trims away", () => {
    expect(sameQuery(buildQuery(form), buildQuery({ ...form, month: " 2026-08 " }))).toBe(true);
  });
  it("sees a changed month, tier, fingerprint or repo", () => {
    expect(sameQuery(buildQuery(form), buildQuery({ ...form, month: "2026-07" }))).toBe(false);
    expect(sameQuery(buildQuery(form), buildQuery({ ...form, tier: "team" }))).toBe(false);
    expect(sameQuery(buildQuery(form), buildQuery({ ...form, fingerprint: "fp1" }))).toBe(false);
    expect(sameQuery(buildQuery(form), buildQuery({ ...form, repo: "acme/app" }))).toBe(false);
  });
});

describe("formatUSD", () => {
  it("renders to cents", () => {
    expect(formatUSD(0)).toBe("$0.00");
    expect(formatUSD(13.6811999)).toBe("$13.68");
  });
});

describe("formatTokens", () => {
  it("shows the input/output split when measured", () => {
    expect(formatTokens({ input_tokens: 1000, output_tokens: 250, aggregate_tokens: 0 })).toBe(
      "1,000 in / 250 out",
    );
  });
  it("shows the aggregate when the split is unavailable", () => {
    expect(formatTokens({ input_tokens: 0, output_tokens: 0, aggregate_tokens: 5000 })).toBe(
      "5,000 (aggregate)",
    );
  });
  // A row merges every repo/backend of one credential-month, so a credential
  // served by both a split-reporting backend and a CLI delegate carries all
  // three counters — dropping the aggregate would under-report the total.
  it("shows the aggregate ALONGSIDE the split when a row carries both", () => {
    expect(formatTokens({ input_tokens: 1000, output_tokens: 250, aggregate_tokens: 5000 })).toBe(
      "1,000 in / 250 out + 5,000 (aggregate)",
    );
  });
  it("keeps a one-sided split rather than calling it an aggregate", () => {
    expect(formatTokens({ input_tokens: 1000, output_tokens: 0, aggregate_tokens: 0 })).toBe(
      "1,000 in / 0 out",
    );
  });
  // Zero everywhere is "not observed", never "none spent" (credusage.go).
  it("says nothing was reported rather than claiming a zero", () => {
    expect(formatTokens({ input_tokens: 0, output_tokens: 0, aggregate_tokens: 0 })).toBe(
      "not reported",
    );
  });
});
