import { describe, expect, it } from "vitest";

import { computePollingInterval } from "./useRuns";

// The cadence helper is pure so the contract — "fewer queued runs →
// fast polling, many queued → slow polling" — can be locked without
// mounting the hook.
describe("computePollingInterval", () => {
  it("returns the fast cadence when no runs are queued", () => {
    expect(computePollingInterval({})).toBe(3000);
  });

  it("returns the fast cadence below the queued threshold", () => {
    expect(computePollingInterval({ queued: 9 })).toBe(3000);
  });

  it("backs off to the slow cadence at the queued threshold", () => {
    expect(computePollingInterval({ queued: 10 })).toBe(8000);
  });

  it("backs off to the slow cadence above the queued threshold", () => {
    expect(computePollingInterval({ queued: 25, running: 3 })).toBe(8000);
  });
});
