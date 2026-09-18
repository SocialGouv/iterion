import { describe, expect, it } from "vitest";

import { runListBodyState, showRefreshingOverlay } from "./runListBodyState";

describe("runListBodyState", () => {
  it("shows the skeleton only on a cold load with no cached rows", () => {
    expect(
      runListBodyState({ loading: true, refreshing: false, error: null, runCount: 0, filteredCount: 0 }),
    ).toBe("skeleton");
  });

  it("shows the skeleton on a switch away from an empty scope (cached [] held)", () => {
    // keepPreviousData holds the previous scope's [] so loading is already
    // false, but the new scope is still fetching (refreshing). Without
    // folding refreshing in this would fall through to the "no runs" CTA.
    expect(
      runListBodyState({ loading: false, refreshing: true, error: null, runCount: 0, filteredCount: 0 }),
    ).toBe("skeleton");
  });

  it("keeps the list (not skeleton) during a cached refetch", () => {
    // loading=true can coincide with cached rows on some transitions; the
    // presence of rows means keepPreviousData is holding them, so we render
    // the list + overlay, never a blank skeleton.
    expect(
      runListBodyState({ loading: true, refreshing: false, error: null, runCount: 5, filteredCount: 5 }),
    ).toBe("list");
  });

  it("keeps the list (not skeleton) while refreshing over cached rows", () => {
    expect(
      runListBodyState({ loading: false, refreshing: true, error: null, runCount: 5, filteredCount: 5 }),
    ).toBe("list");
  });

  it("surfaces errors ahead of empty/list states", () => {
    expect(
      runListBodyState({ loading: false, refreshing: false, error: "boom", runCount: 0, filteredCount: 0 }),
    ).toBe("error");
  });

  it("reports empty when the scope has no runs at all and nothing is loading", () => {
    expect(
      runListBodyState({ loading: false, refreshing: false, error: null, runCount: 0, filteredCount: 0 }),
    ).toBe("empty");
  });

  it("reports no-matches when filters hide every run", () => {
    expect(
      runListBodyState({ loading: false, refreshing: false, error: null, runCount: 8, filteredCount: 0 }),
    ).toBe("no-matches");
  });

  it("renders the list when filtered runs survive", () => {
    expect(
      runListBodyState({ loading: false, refreshing: false, error: null, runCount: 8, filteredCount: 3 }),
    ).toBe("list");
  });
});

describe("showRefreshingOverlay", () => {
  it("shows while a background refetch runs over cached rows", () => {
    expect(
      showRefreshingOverlay({ refreshing: true, repoScopeLoading: false, runCount: 5 }),
    ).toBe(true);
  });

  it("shows while the repo scope is still resolving over cached rows", () => {
    expect(
      showRefreshingOverlay({ refreshing: false, repoScopeLoading: true, runCount: 5 }),
    ).toBe(true);
  });

  it("stays hidden on the cold load (no cached rows to dim)", () => {
    expect(
      showRefreshingOverlay({ refreshing: true, repoScopeLoading: true, runCount: 0 }),
    ).toBe(false);
  });

  it("stays hidden when nothing is loading", () => {
    expect(
      showRefreshingOverlay({ refreshing: false, repoScopeLoading: false, runCount: 5 }),
    ).toBe(false);
  });
});
