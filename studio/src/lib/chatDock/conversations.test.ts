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
  claimRun,
  switchConversationBot,
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

// The bug this closes: after navigating, two tabs showed the SAME conversation
// and the bot-building one was gone. Switching a conversation between active
// and background remounts its session hook, which re-ran the bot-scoped
// lookup — "the latest live run for this bot" — and handed it another
// conversation's run.
//
// Owning the run id is what makes a conversation a conversation rather than a
// view onto whatever ran last.
describe("a conversation owns its run", () => {
  it("survives a round-trip so a remount attaches to the same one", () => {
    writeConversations([{ id: "a", botId: "copilot", runId: "run-a" }]);
    expect(readConversations()[0]?.runId).toBe("run-a");
  });

  it("keeps two conversations on the same bot apart", () => {
    writeConversations([
      { id: "a", botId: "copilot", runId: "run-a" },
      { id: "b", botId: "copilot", runId: "run-b" },
    ]);
    const [a, b] = readConversations();
    expect(a?.runId).toBe("run-a");
    expect(b?.runId).toBe("run-b");
  });

  // Not yet launched: nothing to attach to, and nothing to borrow either.
  it("has none before it launches", () => {
    writeConversations([{ id: "a", botId: "copilot", fresh: true }]);
    expect(readConversations()[0]?.runId).toBeUndefined();
  });
});

// A run has exactly ONE conversation.
//
// Found in a real browser: both tabs carried the SAME runId. Before a
// conversation owned its run, a remount re-ran the bot-scoped lookup, was
// handed a neighbour's run — and that stolen id was then RECORDED on both.
// Storage already holds the broken shape, so the invariant has to be enforced
// on READ (repair) as well as on WRITE (prevent). Repairing on read is what
// makes the fix reach the operator instead of asking them to clear tabs.
describe("one run, one conversation", () => {
  it("repairs storage where two tabs claimed the same run", () => {
    writeConversations([
      { id: "a", botId: "copilot", fresh: false, runId: "shared" },
      { id: "b", botId: "copilot", fresh: false, runId: "shared" },
    ]);
    const [a, b] = readConversations();
    expect(a?.runId).toBe("shared");
    expect(b?.runId).toBeUndefined();
  });

  // Cleared means "has no run", so it must start EMPTY rather than fall back
  // to the lookup — which would hand it the neighbour's run right back.
  it("makes the dispossessed one start fresh", () => {
    writeConversations([
      { id: "a", botId: "copilot", fresh: false, runId: "shared" },
      { id: "b", botId: "copilot", fresh: false, runId: "shared" },
    ]);
    expect(readConversations()[1]?.fresh).toBe(true);
  });

  it("leaves distinct runs alone", () => {
    writeConversations([
      { id: "a", botId: "copilot", runId: "run-a" },
      { id: "b", botId: "copilot", runId: "run-b" },
    ]);
    expect(readConversations().map((x) => x.runId)).toEqual(["run-a", "run-b"]);
  });

  it("refuses to record a run another conversation owns", () => {
    const list = [
      { id: "a", botId: "copilot", runId: "run-a" },
      { id: "b", botId: "copilot" },
    ];
    expect(claimRun(list, "b", "run-a")).toBeNull();
  });

  it("records a run nobody owns", () => {
    const list = [{ id: "a", botId: "copilot", fresh: true }];
    const got = claimRun(list, "a", "run-a");
    expect(got?.[0]).toMatchObject({ runId: "run-a", fresh: false });
  });

  it("lets a conversation re-record its own run", () => {
    const list = [{ id: "a", botId: "copilot", runId: "run-a" }];
    expect(claimRun(list, "a", "run-a")?.[0]?.runId).toBe("run-a");
  });
});

describe("switching the bot on a conversation", () => {
  it("drops the previous bot's run so the new bot cannot attach it", () => {
    const list = [
      { id: "a", botId: "copilot", runId: "run-copi", fresh: false },
      { id: "b", botId: "copilot", runId: "run-other" },
    ];
    const got = switchConversationBot(list, "a", "whats-next");
    expect(got[0]).toMatchObject({ id: "a", botId: "whats-next", fresh: true });
    expect(got[0]?.runId).toBeUndefined();
    expect(got[1]?.runId).toBe("run-other");
  });
});
