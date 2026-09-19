import { describe, expect, it } from "vitest";

import { sameSet } from "./platformCredentials";

describe("sameSet", () => {
  it("is order-independent", () => {
    expect(sameSet(["a", "b"], ["b", "a"])).toBe(true);
  });
  it("detects a differing length or member", () => {
    expect(sameSet(["a"], ["a", "b"])).toBe(false);
    expect(sameSet(["a", "c"], ["a", "b"])).toBe(false);
  });
  it("treats two empty lists as equal", () => {
    expect(sameSet([], [])).toBe(true);
  });
});
