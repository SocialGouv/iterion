// @vitest-environment jsdom
//
// Several conversations at once, each its own run. The rules that matter are
// the ones that decide what the operator is LOOKING at after a change — a tab
// strip that drops you somewhere unexpected is worse than one tab.
import { beforeEach, describe, expect, it } from "vitest";

import {
  CONVERSATIONS_KEY,
  MAX_CONVERSATIONS,
  addConversation,
  closeConversation,
  newConversationId,
  readActiveConversation,
  readConversations,
  resolveActive,
  writeActiveConversation,
  writeConversations,
  type Conversation,
} from "./conversations";

const c = (id: string, botId = "copilot"): Conversation => ({ id, botId });

beforeEach(() => window.localStorage.clear());

describe("persistence", () => {
  it("round-trips a list", () => {
    writeConversations([c("a"), c("b")]);
    expect(readConversations().map((x) => x.id)).toEqual(["a", "b"]);
  });

  it("keeps the origin, which is how you get back to what you were discussing", () => {
    writeConversations([
      { id: "a", botId: "copilot", origin: "run/019f", originLabel: "Run 019f" },
    ]);
    expect(readConversations()[0]).toMatchObject({
      origin: "run/019f",
      originLabel: "Run 019f",
    });
  });

  it("starts empty on a fresh browser", () => {
    expect(readConversations()).toEqual([]);
    expect(readActiveConversation()).toBe("");
  });

  // A corrupt entry must cost its own conversation, never the strip: you
  // cannot be locked out of the dock by bad localStorage.
  it("drops entries it cannot make sense of, keeping the rest", () => {
    window.localStorage.setItem(
      CONVERSATIONS_KEY,
      JSON.stringify([{ id: "a", botId: "copilot" }, { nope: 1 }, null, "x"]),
    );
    expect(readConversations().map((x) => x.id)).toEqual(["a"]);
  });

  it("survives outright garbage", () => {
    window.localStorage.setItem(CONVERSATIONS_KEY, "{{{not json");
    expect(readConversations()).toEqual([]);
  });

  it("mints distinct ids", () => {
    expect(newConversationId()).not.toBe(newConversationId());
  });
});

describe("opening", () => {
  it("appends", () => {
    expect(addConversation([c("a")], c("b")).map((x) => x.id)).toEqual(["a", "b"]);
  });

  // Every open conversation is a live session with its own polling.
  it("refuses to grow past the ceiling", () => {
    const full = Array.from({ length: MAX_CONVERSATIONS }, (_, i) => c(`c${i}`));
    expect(addConversation(full, c("extra"))).toHaveLength(MAX_CONVERSATIONS);
  });
});

describe("closing", () => {
  it("lands on the NEIGHBOUR, the way a tab strip should", () => {
    const list = [c("a"), c("b"), c("c")];
    const got = closeConversation(list, "b", "b");
    expect(got.list.map((x) => x.id)).toEqual(["a", "c"]);
    expect(got.activeId).toBe("c");
  });

  it("falls back to the last one when closing the end", () => {
    const got = closeConversation([c("a"), c("b")], "b", "b");
    expect(got.activeId).toBe("a");
  });

  it("leaves the active one alone when closing another", () => {
    const got = closeConversation([c("a"), c("b")], "a", "b");
    expect(got.activeId).toBe("b");
  });

  it("reports nothing left rather than an error", () => {
    expect(closeConversation([c("a")], "a", "a").activeId).toBeNull();
  });

  it("ignores an id that is not there", () => {
    const got = closeConversation([c("a")], "ghost", "a");
    expect(got.list.map((x) => x.id)).toEqual(["a"]);
    expect(got.activeId).toBe("a");
  });
});

describe("resolving what is on screen", () => {
  it("takes the active one", () => {
    expect(resolveActive([c("a"), c("b")], "b")?.id).toBe("b");
  });

  // Closed in another browser tab, or dropped by a shape change: fall back
  // rather than leave the dock blank.
  it("falls back to the first when the active id is gone", () => {
    expect(resolveActive([c("a"), c("b")], "vanished")?.id).toBe("a");
  });

  it("reports none when there are none", () => {
    expect(resolveActive([], "a")).toBeNull();
  });

  it("remembers which was active", () => {
    writeActiveConversation("b");
    expect(readActiveConversation()).toBe("b");
  });
});

// Closing a tab is not just forgetting it. A conversation is a live agent: if
// the run is not cancelled it keeps burning model spend until a stall watchdog
// or a restart tears it down, and nothing on screen would mention it again.
// The list helper is deliberately pure — the cancel lives in the provider,
// which holds the stores — so what is pinned here is that closing REMOVES the
// entry, i.e. that the caller can no longer reach the run through the strip.
describe("closing releases the conversation", () => {
  it("drops it from the list entirely", () => {
    const got = closeConversation([c("a"), c("b")], "a", "b");
    expect(got.list.find((x) => x.id === "a")).toBeUndefined();
  });

  it("drops the last one, leaving nothing to resume from", () => {
    const got = closeConversation([c("a")], "a", "a");
    expect(got.list).toEqual([]);
    expect(got.activeId).toBeNull();
  });
});

// The reported bug: clicking "+" showed the previous conversation. Discovery
// is keyed on (bot, scope), so a second tab on the same bot was handed the run
// the first one was already showing. A conversation the operator just opened
// is marked fresh and must not attach to anything.
describe("a conversation the operator just opened", () => {
  it("is marked fresh so it does not attach to another tab's run", () => {
    const opened: Conversation = { id: "n", botId: "copilot", fresh: true };
    expect(addConversation([c("a")], opened)[1]?.fresh).toBe(true);
  });

  // Restored from localStorage ≠ just opened: the operator who closed their
  // tab mid-run should still get that run back.
  it("is not fresh once restored from storage", () => {
    writeConversations([{ id: "a", botId: "copilot", fresh: false }]);
    expect(readConversations()[0]?.fresh).toBe(false);
  });

  it("keeps the flag across a round-trip while it is still unlaunched", () => {
    writeConversations([{ id: "a", botId: "copilot", fresh: true }]);
    expect(readConversations()[0]?.fresh).toBe(true);
  });
});
