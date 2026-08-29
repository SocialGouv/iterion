import { describe, expect, it } from "vitest";

import { effectiveModel } from "./effectiveModel";

describe("effectiveModel", () => {
  it("prefers an explicit override", () => {
    expect(effectiveModel("openai/gpt-5.5", "anthropic/claude-opus-5")).toBe(
      "openai/gpt-5.5",
    );
  });

  // The launch form's common case: the operator changed nothing, so the input
  // value is empty and the node's model lives only in the placeholder. A hook
  // keyed on the input alone would show nothing on most launches.
  it("falls back to the authored model on the inherit path", () => {
    expect(effectiveModel("", "anthropic/claude-opus-5")).toBe(
      "anthropic/claude-opus-5",
    );
    expect(effectiveModel(undefined, "anthropic/claude-opus-5")).toBe(
      "anthropic/claude-opus-5",
    );
    expect(effectiveModel("   ", "anthropic/claude-opus-5")).toBe(
      "anthropic/claude-opus-5",
    );
  });

  it("uses the expansion of an env literal", () => {
    expect(
      effectiveModel(undefined, "${CODEX_MODEL:-openai/gpt-5.5}", "openai/gpt-5.5"),
    ).toBe("openai/gpt-5.5");
  });

  // An unexpanded template is not a model id. Passing it on would key a lookup
  // on a string no aggregator has ever published, so the caption would be
  // empty either way — but the miss would also poison the query cache under a
  // meaningless key.
  it("yields nothing while an env literal is unresolved", () => {
    expect(effectiveModel(undefined, "${CODEX_MODEL:-openai/gpt-5.5}")).toBe("");
    expect(
      effectiveModel(undefined, "${A}", "${B}"),
    ).toBe("");
  });

  it("yields nothing when there is no model anywhere", () => {
    expect(effectiveModel(undefined, undefined)).toBe("");
    expect(effectiveModel("", "")).toBe("");
  });
});
