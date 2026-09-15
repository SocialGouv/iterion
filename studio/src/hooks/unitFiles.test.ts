import { describe, expect, it } from "vitest";

import { touchesOpenUnit } from "./unitFiles";

const unit = {
  root: "bots/demo",
  main: "main.bot",
  revision: "r1",
  files: [{ rel: "main.bot" }, { rel: "lib/nodes.bot" }, { rel: "lib/deep/schemas.bot" }],
};

// An edit to a fragment of the open unit is an edit to the open document:
// the merged canvas and the revision a save presents follow it, instead of
// drifting until a 409 at save time.
describe("touchesOpenUnit", () => {
  it("matches the open file, and every fragment of its unit under the root", () => {
    expect(touchesOpenUnit("bots/demo/main.bot", "bots/demo/main.bot", unit)).toBe(true);
    expect(touchesOpenUnit("bots/demo/lib/nodes.bot", "bots/demo/main.bot", unit)).toBe(true);
    expect(touchesOpenUnit("bots/demo/lib/deep/schemas.bot", "bots/demo/main.bot", unit)).toBe(true);
  });

  it("ignores another bot's files, a fragment the unit does not reach, and a closed tab", () => {
    expect(touchesOpenUnit("bots/other/lib/nodes.bot", "bots/demo/main.bot", unit)).toBe(false);
    expect(touchesOpenUnit("bots/demo/lib/unused.bot", "bots/demo/main.bot", unit)).toBe(false);
    expect(touchesOpenUnit("bots/demo/lib/nodes.bot", null, unit)).toBe(false);
    expect(touchesOpenUnit("bots/demo/lib/nodes.bot", "bots/demo/main.bot", null)).toBe(false);
  });

  it("names a cloud bundle's files from its root", () => {
    const cloud = { ...unit, root: "" };
    expect(touchesOpenUnit("lib/nodes.bot", "main.bot", cloud)).toBe(true);
    expect(touchesOpenUnit("bots/demo/lib/nodes.bot", "main.bot", cloud)).toBe(false);
  });
});
