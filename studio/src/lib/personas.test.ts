import { describe, expect, it } from "vitest";

import { botIdentity, botVisual } from "./personas";

// Known-good built-in persona entry (whats-next ships in the persona map).
const whatsNext = botIdentity("whats-next");

describe("botVisual", () => {
  it("prefers the manifest icon over the built-in persona map", () => {
    const v = botVisual({ name: "whats-next", icon: "🦉" });
    expect(v.emoji).toBe("🦉");
    // …but the colour still comes from the persona path.
    expect(v.color).toBe(whatsNext.color);
  });

  it("prefers the manifest icon over the hash fallback for unknown bots", () => {
    const v = botVisual({ name: "totally-unknown-bot", icon: "🐙" });
    expect(v.emoji).toBe("🐙");
    expect(v.color).toBe(botIdentity("totally-unknown-bot").color);
  });

  it("falls back to the built-in persona map when no icon is set", () => {
    expect(botVisual({ name: "whats-next" })).toEqual(whatsNext);
    // Blank icons don't count as set.
    expect(botVisual({ name: "whats-next", icon: "  " })).toEqual(whatsNext);
  });

  it("falls back to the generic hash identity for unknown bots without an icon", () => {
    const v = botVisual({ name: "totally-unknown-bot" });
    expect(v.emoji).toBe("🤖");
    expect(v).toEqual(botIdentity("totally-unknown-bot"));
  });
});
