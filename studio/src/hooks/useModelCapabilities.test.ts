import { describe, expect, it } from "vitest";

import { modelCapsStaleTime } from "./useModelCapabilities";

// Spec resolution is non-blocking by design: a cold lookup answers from the
// curated table and only STARTS the background refresh, installing the fetched
// prices moments later. The sibling effort-capabilities hook pins its answers
// for the whole session, and copying that here would freeze the first,
// price-less answer — so the tooltip would never show a price on a cold start,
// which is the entire feature. A developer with a warm cache would never see
// it fail.
describe("modelCapsStaleTime", () => {
  it("keeps an aggregator answer for the session — it cannot improve", () => {
    expect(modelCapsStaleTime("aggregator")).toBe(Number.POSITIVE_INFINITY);
  });

  it("lets a curated answer go stale — the refresh may still land", () => {
    const curated = modelCapsStaleTime("curated");
    expect(Number.isFinite(curated)).toBe(true);
    expect(curated).toBeGreaterThan(0);
  });

  // No data yet is the pre-refresh state, so it must behave like curated
  // rather than inherit the never-refetch rule.
  it("treats an absent answer as improvable", () => {
    expect(modelCapsStaleTime(undefined)).toBe(modelCapsStaleTime("curated"));
  });
});
