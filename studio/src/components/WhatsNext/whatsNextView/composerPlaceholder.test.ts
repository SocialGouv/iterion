import { describe, expect, it } from "vitest";

import { composerPlaceholder } from "./composerPlaceholder";

describe("composerPlaceholder", () => {
  // The regression this pins was found by driving the real dock: switching
  // the assistant to Copi left the composer offering to message Nexie. The
  // copy was a literal, which was harmless while the chat surface could only
  // host one bot and wrong the moment the registry became manifest-driven —
  // a switch whose only visible surface still names the other bot reads as a
  // switch that did nothing.
  it("names the bot the operator actually picked", () => {
    expect(composerPlaceholder(null, false, "Copi")).toContain("Copi");
    expect(composerPlaceholder(null, false, "Copi")).not.toContain("Nexie");
    expect(composerPlaceholder(null, true, "Copi")).toBe("Reply to Copi…");
    expect(composerPlaceholder("finished", false, "Copi")).toContain(
      "fresh Copi session",
    );
  });

  it("falls back to a neutral name rather than a stale one", () => {
    // A caller with no bot resolved must not silently re-introduce a persona.
    for (const label of [undefined, "", "   "]) {
      const out = composerPlaceholder(null, false, label);
      expect(out).toContain("the assistant");
      expect(out).not.toContain("Nexie");
    }
  });

  it("keeps the three states distinct", () => {
    const live = composerPlaceholder(null, false, "Nexie");
    const pending = composerPlaceholder(null, true, "Nexie");
    const terminal = composerPlaceholder("cancelled", false, "Nexie");
    expect(new Set([live, pending, terminal]).size).toBe(3);
  });
});
