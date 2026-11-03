import { describe, expect, it } from "vitest";

import { effectiveKind } from "./botBindings";

// The discriminator is absent on every binding stored before kinds existed
// (ADR-147 D2): the UI must never read the raw field, or a pre-kinds
// binding renders as a context with no source.
describe("effectiveKind", () => {
  it("reads an absent kind as secret", () => {
    expect(effectiveKind({})).toBe("secret");
  });
  it("reads an explicit secret kind as secret", () => {
    expect(effectiveKind({ kind: "secret" })).toBe("secret");
  });
  it("reads the context kind as context", () => {
    expect(effectiveKind({ kind: "context" })).toBe("context");
  });
  it("never invents a kind from an unknown value", () => {
    expect(effectiveKind({ kind: "wishful" })).toBe("secret");
  });
});
