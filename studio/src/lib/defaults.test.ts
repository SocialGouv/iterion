import { describe, expect, it } from "vitest";
import { createEmptyDocument } from "./defaults";

describe("createEmptyDocument", () => {
  it("is written in syntax profile 2, so the saved file opens with `dsl: 2`", () => {
    expect(createEmptyDocument().profile).toBe(2);
  });
});
