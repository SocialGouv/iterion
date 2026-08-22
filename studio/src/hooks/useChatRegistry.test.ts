import { describe, expect, it } from "vitest";

import { FIRST_CLASS_BOTS } from "@/lib/whats-next/firstClassBots";

import { chatRegistryWithFloor, resolveChatBot } from "./useChatRegistry";

import type { BotEntry } from "@/api/bots";

describe("resolveChatBot", () => {
  const defaultBot = FIRST_CLASS_BOTS["whats-next"]!;

  it("parks a persisted unknown id while registry discovery is loading", () => {
    expect(
      resolveChatBot(FIRST_CLASS_BOTS, [defaultBot], "copilot", true),
    ).toBeNull();
  });

  it("falls back only after discovery proves the persisted id is unknown", () => {
    expect(
      resolveChatBot(FIRST_CLASS_BOTS, [defaultBot], "missing", false),
    ).toBe(defaultBot);
  });

  it("can resolve the built-in floor during loading", () => {
    expect(
      resolveChatBot(FIRST_CLASS_BOTS, [defaultBot], "whats-next", true),
    ).toBe(defaultBot);
  });
});

// The built-in floor exists for "discovery could not answer". An operator who
// turned a bot OFF in the Catalog manager is discovery answering "no" — and
// /api/v1/bots reports those entries rather than omitting them, so the floor
// must not resurrect one. For the default id the stake is higher than one
// stray row: resolveChatBot picks it as the default correspondent.
describe("chatRegistryWithFloor", () => {
  const entry = (over: Partial<BotEntry>): BotEntry =>
    ({ name: "x", path: "/w/bots/x", ...over }) as BotEntry;

  it("keeps the built-in floor when discovery returns nothing", () => {
    expect(Object.keys(chatRegistryWithFloor([]))).toContain("whats-next");
  });

  it("keeps the floor for a bot the listing reports as enabled", () => {
    const reg = chatRegistryWithFloor([
      entry({ name: "whats-next", enabled: true }),
    ]);
    expect(reg["whats-next"]).toBe(FIRST_CLASS_BOTS["whats-next"]);
  });

  it("drops the floor for a bot the operator disabled in the Catalog", () => {
    const reg = chatRegistryWithFloor([
      entry({ name: "whats-next", enabled: false }),
    ]);
    expect(reg["whats-next"]).toBeUndefined();
  });

  it("stops a disabled default from outranking the bots still enabled", () => {
    const reg = chatRegistryWithFloor([
      entry({ name: "whats-next", enabled: false }),
    ]);
    const bots = Object.values(reg);
    expect(resolveChatBot(reg, bots, null, false)?.id).not.toBe("whats-next");
  });
});
