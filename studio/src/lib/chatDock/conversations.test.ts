// @vitest-environment jsdom
//
// Several conversations at once, each its own run. The rules that matter are
// the ones that decide what the operator is LOOKING at after a change — a tab
// strip that drops you somewhere unexpected is worse than one tab.
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import {
  CONVERSATIONS_KEY,
  MAX_CONVERSATIONS,
  addConversation,
  anchorConversation,
  closeConversation,
  conversationContextState,
  newConversationId,
  readActiveConversation,
  readConversations,
  resolveActive,
  writeActiveConversation,
  writeConversations,
  type Conversation,
  claimRun,
  collectUnreadWatchConversationIds,
  collectWaitingConversationIds,
  latestWatchResultSeq,
  markConversationContextUnknown,
  markConversationWatchResultRead,
  setConversationContextEnabled,
  switchConversationBot,
} from "./conversations";

const c = (id: string, botId = "copilot"): Conversation => ({ id, botId });

beforeEach(() => window.localStorage.clear());

describe("persistence", () => {
  it("round-trips a list", () => {
    writeConversations([c("a"), c("b")]);
    expect(readConversations().map((x) => x.id)).toEqual(["a", "b"]);
  });

  it("round-trips the empty bot id used for the registry default", () => {
    writeConversations([{ id: "default", botId: "", runId: "run-default" }]);
    expect(readConversations()).toEqual([
      { id: "default", botId: "", runId: "run-default" },
    ]);
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

  it("drops entries whose optional navigation fields have corrupt types", () => {
    window.localStorage.setItem(
      CONVERSATIONS_KEY,
      JSON.stringify([
        { id: "good", botId: "copilot", origin: "view/board" },
        { id: "bad-origin", botId: "copilot", origin: { href: "/admin" } },
        { id: "bad-run", botId: "copilot", runId: 42 },
        { id: "bad-fresh", botId: "copilot", fresh: "yes" },
        {
          id: "bad-href",
          botId: "copilot",
          originHref: "/\\attacker.example/path",
        },
      ]),
    );
    expect(readConversations().map((x) => x.id)).toEqual(["good"]);
  });

  it("survives outright garbage", () => {
    window.localStorage.setItem(CONVERSATIONS_KEY, "{{{not json");
    expect(readConversations()).toEqual([]);
  });

  it("mints distinct ids", () => {
    expect(newConversationId()).not.toBe(newConversationId());
  });
});

describe("workspace persistence", () => {
  afterEach(() => {
    delete (globalThis as { __ITERION_SCOPE__?: string }).__ITERION_SCOPE__;
  });

  it("isolates run ownership between project panes on the same origin", () => {
    (globalThis as { __ITERION_SCOPE__?: string }).__ITERION_SCOPE__ = "/x/town";
    writeConversations([{ id: "town-chat", botId: "copilot", runId: "town-run" }]);

    (globalThis as { __ITERION_SCOPE__?: string }).__ITERION_SCOPE__ =
      "/x/tabarria";
    expect(readConversations()).toEqual([]);
    writeConversations([
      { id: "tabarria-chat", botId: "copilot", runId: "tabarria-run" },
    ]);

    (globalThis as { __ITERION_SCOPE__?: string }).__ITERION_SCOPE__ = "/x/town";
    expect(readConversations()).toEqual([
      { id: "town-chat", botId: "copilot", runId: "town-run" },
    ]);
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

describe("the immutable first-message context", () => {
  it("treats an old creation-time origin as pending until the tab starts", () => {
    const legacy = {
      id: "a",
      botId: "copilot",
      fresh: true,
      origin: "view/board",
    };
    expect(conversationContextState(legacy, false)).toBe("pending");
  });

  it("anchors once and refuses a later page", () => {
    const first = anchorConversation([c("a")], "a", {
      ref: "run/a",
      label: "Run a",
      href: "/runs/a?tab=events",
    });
    const second = anchorConversation(first, "a", {
      ref: "view/board",
      label: "Board",
      href: "/board",
    });
    expect(second[0]).toMatchObject({
      origin: "run/a",
      originLabel: "Run a",
      originHref: "/runs/a?tab=events",
      contextState: "anchored",
    });
  });

  it("lets migration replace the old unmarked creation-time origin", () => {
    const got = anchorConversation(
      [{ ...c("a"), runId: "r", origin: "view/board" }],
      "a",
      { ref: "run/r", label: "Run r", href: "/runs/r" },
    );
    expect(got[0]).toMatchObject({
      origin: "run/r",
      originHref: "/runs/r",
      contextState: "anchored",
    });
  });

  it("keeps opt-out scoped to one empty conversation", () => {
    const disabled = setConversationContextEnabled([c("a"), c("b")], "a", false);
    expect(disabled[0]?.contextState).toBe("disabled");
    expect(conversationContextState(disabled[1]!, false)).toBe("pending");
    const restored = setConversationContextEnabled(disabled, "a", true);
    expect(restored[0]?.contextState).toBe("pending");
  });

  it("records an unrecoverable legacy context explicitly", () => {
    expect(
      markConversationContextUnknown([{ ...c("a"), runId: "r" }], "a")[0]
        ?.contextState,
    ).toBe("unknown");
  });

  it("keeps a started pending record pending while its transcript loads", () => {
    const interrupted = {
      ...c("a"),
      runId: "r",
      contextState: "pending" as const,
    };
    expect(conversationContextState(interrupted, false)).toBe("pending");
    expect(
      markConversationContextUnknown([interrupted], "a")[0]?.contextState,
    ).toBe("unknown");
  });

  it("does not mistake a legacy run id for an unavailable context", () => {
    expect(
      conversationContextState({ ...c("a"), runId: "r" }, false),
    ).toBe("pending");
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

describe("human-gate attention", () => {
  const conversations: Conversation[] = [
    { id: "active", botId: "copilot", runId: "run-active" },
    { id: "background", botId: "copilot", runId: "run-background" },
    { id: "operator", botId: "copilot", runId: "run-operator" },
    { id: "empty", botId: "copilot" },
  ];

  it("counts each active or background tab parked on its own human gate", () => {
    const states = new Map([
      ["active", { runId: "run-active", runStatus: "running" }],
      [
        "background",
        { runId: "run-background", runStatus: "paused_waiting_human" },
      ],
      ["operator", { runId: "run-operator", runStatus: "paused_operator" }],
      ["empty", { runId: null, runStatus: null }],
    ]);
    expect(
      Array.from(
        collectWaitingConversationIds(
          conversations,
          (id) => states.get(id) ?? { runId: null, runStatus: null },
        ),
      ),
    ).toEqual(["background"]);
  });

  it("refuses a paused run that belongs to another tab", () => {
    expect(
      collectWaitingConversationIds(conversations.slice(0, 1), () => ({
        runId: "neighbour",
        runStatus: "paused_waiting_human",
      })).size,
    ).toBe(0);
  });
});

describe("automatic watch unread state", () => {
  const event = (
    seq: number,
    type: "human_answers_recorded" | "human_input_requested",
    nodeId: string,
    data: Record<string, unknown>,
  ) => ({
    seq,
    type,
    node_id: nodeId,
    run_id: "assistant",
    timestamp: `2026-08-30T09:00:${String(seq).padStart(2, "0")}Z`,
    data,
  });

  it("distinguishes a new watch diagnosis from the ordinary chat pause", () => {
    const events = [
      event(10, "human_input_requested", "chat", {}),
      event(11, "human_answers_recorded", "chat", {
        answers: {
          host_event: {
            kind: "assistant-watch-event",
            event: "run.failed",
          },
        },
      }),
      event(12, "human_input_requested", "chat", {}),
    ];
    expect(latestWatchResultSeq(events)).toBe(12);

    const conversations: Conversation[] = [
      { id: "copi", botId: "copilot", runId: "assistant" },
    ];
    expect(
      collectUnreadWatchConversationIds(conversations, () => ({
        runId: "assistant",
        runStatus: "paused_waiting_human",
        latestWatchResultSeq: 12,
      })),
    ).toEqual(new Set(["copi"]));

    const read = markConversationWatchResultRead(conversations, "copi", 12);
    expect(
      collectUnreadWatchConversationIds(read, () => ({
        runId: "assistant",
        runStatus: "paused_waiting_human",
        latestWatchResultSeq: 12,
      })).size,
    ).toBe(0);
  });

  it("waits for the finalized reply instead of alerting while Copi is working", () => {
    const events = [
      event(20, "human_answers_recorded", "chat", {
        answers: { host_event: { kind: "assistant-watch-event" } },
      }),
      // An ask_user pause inside the turn is a different node and must not
      // publish the automatic diagnosis prematurely.
      event(21, "human_input_requested", "copi", {}),
    ];
    expect(latestWatchResultSeq(events)).toBeNull();
  });

  it("treats a completed project handoff reply as an unread assistant update", () => {
    const events = [
      event(30, "human_answers_recorded", "chat", {
        answers: {
          host_event: {
            kind: "workspace-handoff-completed",
            receipt_id: "receipt-1",
          },
        },
      }),
      event(31, "human_input_requested", "chat", {}),
    ];
    expect(latestWatchResultSeq(events)).toBe(31);
  });

  it("persists the monotonic read cursor", () => {
    const read = markConversationWatchResultRead(
      [{ id: "copi", botId: "copilot", runId: "assistant" }],
      "copi",
      42,
    );
    writeConversations(read);
    expect(readConversations()[0]?.lastReadWatchResultSeq).toBe(42);
    expect(
      markConversationWatchResultRead(read, "copi", 41)[0]
        ?.lastReadWatchResultSeq,
    ).toBe(42);
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
