import { describe, expect, it } from "vitest";

import {
  formatContextWindow,
  formatModelPrice,
  modelCapabilityWarning,
  type ModelEntry,
} from "./models";

function entry(patch: Partial<ModelEntry> = {}): ModelEntry {
  return {
    spec: "anthropic/claude-opus-5",
    provider: "anthropic",
    model: "claude-opus-5",
    credential_provider: "anthropic",
    source: "curated",
    context_window: 200_000,
    reasoning: true,
    tool_call: true,
    temperature: true,
    ultracode_capable: false,
    price_known: true,
    input_cost_per_m: 15,
    output_cost_per_m: 75,
    usable: true,
    ...patch,
  };
}

describe("formatContextWindow", () => {
  it("renders compactly and falls back to an em-dash when unknown", () => {
    expect(formatContextWindow(1_000_000)).toBe("1M");
    expect(formatContextWindow(200_000)).toBe("200K");
    expect(formatContextWindow(4096)).toBe("4096");
    expect(formatContextWindow(0)).toBe("—");
  });
});

describe("formatModelPrice", () => {
  // A zero rate means no source published one. Rendering it as "$0" would
  // read as free, which is the one thing it never is.
  it("shows an em-dash rather than implying a model is free", () => {
    expect(formatModelPrice(entry({ price_known: false }))).toBe("—");
    expect(
      formatModelPrice(
        entry({ price_known: false, input_cost_per_m: 0, output_cost_per_m: 0 }),
      ),
    ).toBe("—");
  });

  it("renders both rates per million tokens", () => {
    expect(formatModelPrice(entry())).toBe("$15 / $75 per Mtok");
  });

  it("keeps decimals on sub-dollar rates", () => {
    const cheap = entry({ input_cost_per_m: 0.25, output_cost_per_m: 2 });
    expect(formatModelPrice(cheap)).toBe("$0.25 / $2 per Mtok");
  });
});

describe("modelCapabilityWarning", () => {
  it("says nothing about a model that is fine", () => {
    expect(modelCapabilityWarning(entry())).toBeNull();
  });

  it("blocks on a model this host cannot reach, and repeats the server's reason", () => {
    const w = modelCapabilityWarning(
      entry({ usable: false, unusable_reason: "no credential detected for provider openai" }),
    );
    expect(w?.level).toBe("blocking");
    expect(w?.message).toContain("openai");
  });

  // Tool-calling is the capability whose absence breaks the assistant
  // outright — no board, no skills, no run introspection. It has to be
  // surfaced before launch, not discovered mid-run.
  it("blocks on a model that cannot call tools", () => {
    const w = modelCapabilityWarning(entry({ tool_call: false }));
    expect(w?.level).toBe("blocking");
    expect(w?.message).toContain("tools");
  });

  it("prefers the unreachable reason over the tool-call one", () => {
    const w = modelCapabilityWarning(
      entry({ usable: false, tool_call: false, unusable_reason: "no credential" }),
    );
    expect(w?.message).toBe("no credential");
  });

  // ultracode is a silent downgrade, not a breakage: warn, never block.
  it("warns that ultracode degrades off claude-opus-4-8", () => {
    const w = modelCapabilityWarning(entry({ ultracode_capable: false }), {
      wantsUltracode: true,
    });
    expect(w?.level).toBe("warning");
    expect(w?.message).toContain("xhigh");
  });

  it("stays quiet about ultracode on a model that supports it", () => {
    expect(
      modelCapabilityWarning(
        entry({ spec: "anthropic/claude-opus-4-8", ultracode_capable: true }),
        { wantsUltracode: true },
      ),
    ).toBeNull();
  });

  it("stays quiet about ultracode when the node does not ask for it", () => {
    expect(modelCapabilityWarning(entry({ ultracode_capable: false }))).toBeNull();
  });

  it("has nothing to say about a model it has no entry for", () => {
    expect(modelCapabilityWarning(undefined)).toBeNull();
  });
});
