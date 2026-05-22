import { describe, expect, it } from "vitest";

import { budgetPillTooltip } from "./RunMetrics";

describe("budgetPillTooltip", () => {
  it("describes the dimension, the percentage, and the used/limit", () => {
    const out = budgetPillTooltip(
      { dimension: "cost_usd", ratio: 0.42, used: 4.2, limit: 10 },
      false,
    );
    expect(out).toMatch(/cost_usd at 42% \(4\.2 \/ 10\)/);
  });

  it("warns the run will stop when the hard cap is hit (not-yet-exceeded)", () => {
    const out = budgetPillTooltip(
      { dimension: "tokens", ratio: 0.8, used: 800, limit: 1000 },
      false,
    );
    expect(out).toMatch(/Run will stop when the hard cap is hit\./);
  });

  it("announces the hard cap as reached when exceeded", () => {
    const out = budgetPillTooltip(
      { dimension: "iterations", ratio: 1, used: 5, limit: 5 },
      true,
    );
    expect(out).toMatch(/Hard cap reached\.$/);
  });

  it("rounds the percentage to the nearest integer", () => {
    const out = budgetPillTooltip(
      { dimension: "duration", ratio: 0.756, used: 7560, limit: 10000 },
      false,
    );
    expect(out).toMatch(/at 76%/);
  });
});
