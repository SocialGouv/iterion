import { describe, expect, it } from "vitest";

import type { ModelCapabilities } from "@/api/client";
import { formatPricePair, formatTokens, modelCapsTooltip } from "./modelCapsTooltip";

function caps(over: Partial<ModelCapabilities> = {}): ModelCapabilities {
  return {
    spec: "anthropic/claude-opus-5",
    model: "claude-opus-5",
    source: "aggregator",
    context_window: 0,
    max_output_tokens: 0,
    input_cost_per_m: 0,
    output_cost_per_m: 0,
    ...over,
  };
}

describe("formatTokens", () => {
  it("renders the compact units the CLI table uses", () => {
    expect(formatTokens(1_000_000)).toBe("1M");
    expect(formatTokens(200_000)).toBe("200K");
    expect(formatTokens(4096)).toBe("4096");
  });

  it("renders nothing for a zero or nonsense count — zero means unknown", () => {
    expect(formatTokens(0)).toBe("");
    expect(formatTokens(-1)).toBe("");
    expect(formatTokens(Number.NaN)).toBe("");
  });
});

describe("formatPricePair", () => {
  it("renders a fully published pair", () => {
    expect(formatPricePair(5, 25)).toBe("$5.00 / $25.00 per M");
  });

  // The two rates are published independently, and the cost estimator refuses
  // a half pair whole. The caption must agree with it — printing "$5.00 /
  // $0.00" would advertise a price no run is charged at, with the missing half
  // reading as free.
  it("calls a half-published pair unknown, matching the estimator", () => {
    expect(formatPricePair(5, 0)).toBe("price unknown");
    expect(formatPricePair(0, 25)).toBe("price unknown");
    expect(formatPricePair(0, 0)).toBe("price unknown");
  });
});

describe("modelCapsTooltip", () => {
  it("assembles the full caption", () => {
    expect(
      modelCapsTooltip(
        caps({
          context_window: 1_000_000,
          max_output_tokens: 64_000,
          input_cost_per_m: 5,
          output_cost_per_m: 25,
        }),
      ),
    ).toBe("1M context · 64K max out · $5.00 / $25.00 per M · aggregator");
  });

  it("omits an unknown segment rather than printing a zero", () => {
    const got = modelCapsTooltip(
      caps({ source: "curated", context_window: 200_000 }),
    );
    expect(got).toBe("200K context · price unknown · curated");
    expect(got).not.toContain("max out");
    expect(got).not.toContain("$0.00");
  });

  it("renders nothing at all without capabilities", () => {
    expect(modelCapsTooltip(null)).toBe("");
    expect(modelCapsTooltip(undefined)).toBe("");
  });

  // A model no source carries at all. "price unknown · curated" would be a
  // line under the picker whose only content is that iterion has nothing to
  // say, so the caption stays absent instead.
  it("renders nothing when no field is known", () => {
    expect(modelCapsTooltip(caps({ source: "curated" }))).toBe("");
  });

  // A published price with no limits is still worth a caption.
  it("renders a price-only caption", () => {
    expect(
      modelCapsTooltip(caps({ input_cost_per_m: 1, output_cost_per_m: 5 })),
    ).toBe("$1.00 / $5.00 per M · aggregator");
  });
});
