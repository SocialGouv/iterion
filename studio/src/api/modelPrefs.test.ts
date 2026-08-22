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

  // Selector "*" — every LLM node. A conversational session is one agent from
  // the operator's point of view, whatever the bot's graph looks like.
  it("targets every LLM node with the chosen dimensions", () => {
    expect(
      modelPrefOverrides({
        model: "anthropic/claude-opus-5",
        backend: "claude_code",
        effort: "ultracode",
      }),
    ).toEqual([
      {
        selector: "*",
        model: "anthropic/claude-opus-5",
        backend: "claude_code",
        effort: "ultracode",
      },
    ]);
  });

  it("omits the dimensions left on the bot default", () => {
    expect(modelPrefOverrides({ effort: "high" })).toEqual([
      { selector: "*", model: undefined, backend: undefined, effort: "high" },
    ]);
  });
});
