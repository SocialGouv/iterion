import { describe, expect, it } from "vitest";

import { nodeModelSpecs } from "./nodeModelSpecs";

describe("nodeModelSpecs", () => {
  it("collects distinct pinned specs, sorted", () => {
    expect(
      nodeModelSpecs([
        { model: "openai/gpt-5.5" },
        { model: "anthropic/claude-opus-5" },
        { model: "openai/gpt-5.5" },
      ]),
    ).toEqual(["anthropic/claude-opus-5", "openai/gpt-5.5"]);
  });

  // A ${VAR} default is a launch-time placeholder. Passing it to the registry
  // would either 400 (no "/") or resolve a model that does not exist.
  it("drops ${VAR} placeholders and empties", () => {
    expect(
      nodeModelSpecs([
        { model: "${MODEL}" },
        { model: "anthropic/${TIER}" },
        { model: "" },
        { model: "   " },
      ]),
    ).toEqual([]);
  });
});
