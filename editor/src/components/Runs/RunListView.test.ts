import { describe, expect, it } from "vitest";

import { computePollingInterval } from "./RunListView";

// Lightweight unit test for the cadence helper. The full RunListView
// is a React tree we don't need to mount to lock the contract — the
// only behavioural switch is "fewer queued runs → fast polling, many
// queued → slow polling" which the helper isolates from JSX so the
// effect can stay readable.
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
