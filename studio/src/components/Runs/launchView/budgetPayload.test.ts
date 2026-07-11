import { describe, expect, it } from "vitest";

import {
  buildBudgetPayload,
  emptyBudgetFieldValues,
} from "./budgetPayload";

describe("buildBudgetPayload", () => {
  it("returns undefined when every field is empty (inherit from the bot)", () => {
    expect(buildBudgetPayload(emptyBudgetFieldValues)).toBeUndefined();
  });

  it("returns undefined for whitespace-only inputs", () => {
    expect(
      buildBudgetPayload({
        ...emptyBudgetFieldValues,
        maxCostUsd: "  ",
        maxDuration: "   ",
      }),
    ).toBeUndefined();
  });

  it("builds a partial object with only the fields that are set", () => {
    expect(
      buildBudgetPayload({
        ...emptyBudgetFieldValues,
        maxCostUsd: "2.5",
        maxDuration: "2h",
      }),
    ).toEqual({ max_cost_usd: 2.5, max_duration: "2h" });
  });

  it("builds the full object when everything is set", () => {
    expect(
      buildBudgetPayload({
        maxCostUsd: "120",
        maxTokens: "500000",
        maxDuration: "4h",
        maxIterations: "12",
        maxParallelBranches: "3",
      }),
    ).toEqual({
      max_cost_usd: 120,
      max_tokens: 500000,
      max_duration: "4h",
      max_iterations: 12,
      max_parallel_branches: 3,
    });
  });

  it("floors fractional integer dimensions", () => {
    expect(
      buildBudgetPayload({ ...emptyBudgetFieldValues, maxIterations: "3.7" }),
    ).toEqual({ max_iterations: 3 });
  });

  it("treats zero, negative, and non-numeric values as inherit", () => {
    expect(
      buildBudgetPayload({
        ...emptyBudgetFieldValues,
        maxCostUsd: "0",
        maxTokens: "-5",
        maxIterations: "abc",
      }),
    ).toBeUndefined();
  });

  it("passes max_duration through verbatim for server-side validation", () => {
    // A bad Go duration must reach the server (which 400s), not be
    // silently dropped by the client.
    expect(
      buildBudgetPayload({ ...emptyBudgetFieldValues, maxDuration: "4 hours" }),
    ).toEqual({ max_duration: "4 hours" });
  });
});
