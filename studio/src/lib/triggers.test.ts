import { describe, expect, it } from "vitest";

import type { BotEntry } from "@/api/bots";

import { triggerChips } from "./triggers";

function bot(invocations: BotEntry["invocations"]): BotEntry {
  return { name: "b", invocations } as BotEntry;
}

describe("triggerChips", () => {
  it("names the package trigger a connector invocation suggests", () => {
    const b = bot([{ kind: "connector", connector: { trigger: "linear.issue.created" } }]);
    expect(triggerChips(b)).toEqual(["linear.issue.created"]);
  });
});
