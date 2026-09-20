import { describe, expect, it } from "vitest";

import {
  BOT_CATEGORIES,
  groupBotsByCategory,
  presetDisplayName,
} from "@/lib/botTaxonomy";

describe("groupBotsByCategory", () => {
  it("orders groups in the canonical lifecycle order, Uncategorized last", () => {
    const groups = groupBotsByCategory([
      { name: "nexie", category: "steer" },
      { name: "revi", category: "verify" },
      { name: "featurly", category: "build" },
      { name: "doki", category: "document" },
      { name: "envy", category: "operate" },
      { name: "renovacy", category: "harden" },
      { name: "loose", category: undefined },
    ]);
    expect(groups.map((g) => g.category.slug)).toEqual([
      "build",
      "verify",
      "harden",
      "document",
      "operate",
      "steer",
      "",
    ]);
    const uncat = groups[groups.length - 1];
    expect(uncat?.category.slug).toBe("");
    expect(uncat?.bots.map((b) => b.name)).toEqual(["loose"]);
  });

  it("drops the Uncategorized group when empty but keeps the six landmarks", () => {
    const groups = groupBotsByCategory([{ name: "revi", category: "verify" }]);
    expect(groups).toHaveLength(6);
    expect(groups.map((g) => g.category.slug)).not.toContain("");
    // Empty canonical groups are kept: stable landmarks, count 0.
    expect(groups.find((g) => g.category.slug === "build")?.bots).toEqual([]);
  });

  it("routes an UNKNOWN slug to Uncategorized, visibly", () => {
    const groups = groupBotsByCategory([{ name: "odd", category: "pilot" }]);
    const uncat = groups.find((g) => g.category.slug === "");
    expect(uncat?.bots.map((b) => b.name)).toEqual(["odd"]);
  });

  it("keeps input order within a group", () => {
    const groups = groupBotsByCategory([
      { name: "b", category: "verify" },
      { name: "a", category: "verify" },
    ]);
    expect(groups.find((g) => g.category.slug === "verify")?.bots.map((b) => b.name)).toEqual([
      "b",
      "a",
    ]);
  });
});

describe("the vocabulary contract", () => {
  it("declares exactly the closed six slugs in lifecycle order", () => {
    expect(BOT_CATEGORIES.map((c) => c.slug)).toEqual([
      "build",
      "verify",
      "harden",
      "document",
      "operate",
      "steer",
    ]);
    for (const c of BOT_CATEGORIES) {
      expect(c.title).toBeTruthy();
      expect(c.tagline).toBeTruthy();
    }
  });

  it("falls back from display_name to the preset name", () => {
    expect(presetDisplayName({ display_name: "  Strict  ", name: "strict" })).toBe("Strict");
    expect(presetDisplayName({ display_name: "   ", name: "strict" })).toBe("strict");
    expect(presetDisplayName({ name: "strict" })).toBe("strict");
  });
});
