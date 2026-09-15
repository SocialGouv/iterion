import { describe, expect, it } from "vitest";

import { modelPrefOverrides } from "./modelPrefs";

describe("modelPrefOverrides", () => {
  // No choice must send nothing at all, so the bot's own DSL defaults apply
  // untouched. Sending an entry of empty strings would be a different thing.
  it("sends nothing when nothing is chosen", () => {
    expect(modelPrefOverrides(null)).toBeUndefined();
    expect(modelPrefOverrides(undefined)).toBeUndefined();
    expect(modelPrefOverrides({})).toBeUndefined();
    expect(
      modelPrefOverrides({ model: "  ", backend: "", effort: undefined }),
    ).toBeUndefined();
  });

  // Selector "agent" changes the answering model without collapsing an
  // explicitly cross-family judge onto the same choice.
  it("targets agent nodes with the chosen dimensions", () => {
    expect(
      modelPrefOverrides({
        model: "anthropic/claude-opus-5",
        backend: "claude_code",
        effort: "ultracode",
      }),
    ).toEqual([
      {
        selector: "agent",
        model: "anthropic/claude-opus-5",
        backend: "claude_code",
        effort: "ultracode",
      },
    ]);
  });

  it("omits the dimensions left on the bot default", () => {
    expect(modelPrefOverrides({ effort: "high" })).toEqual([
      {
        selector: "agent",
        model: undefined,
        backend: undefined,
        effort: "high",
      },
    ]);
  });
});
