import { describe, expect, it } from "vitest";

import { FIRST_CLASS_BOTS } from "@/lib/whats-next/firstClassBots";

import { resolveChatBot } from "./useChatRegistry";

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
