import { describe, expect, it } from "vitest";

import { computePollingInterval } from "./useRuns";

// The cadence helper is pure so the contract — "active runs → fast,
// deep queue → slow, all-terminal/empty → idle" — can be locked
// without mounting the hook.
describe("computePollingInterval", () => {
  it("returns the idle cadence when the list is empty", () => {
    expect(computePollingInterval({})).toBe(15000);
  });

  it("returns the idle cadence when every run is terminal", () => {
    expect(computePollingInterval({ finished: 12, failed: 2, cancelled: 1 })).toBe(15000);
  });

  it("returns the fast cadence while runs are active", () => {
    expect(computePollingInterval({ running: 1 })).toBe(3000);
    expect(computePollingInterval({ queued: 1, finished: 4 })).toBe(3000);
    expect(computePollingInterval({ paused_waiting_human: 2 })).toBe(3000);
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
