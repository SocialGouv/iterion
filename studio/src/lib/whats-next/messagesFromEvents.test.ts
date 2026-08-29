import { describe, it, expect } from "vitest";

import type { RunEvent } from "@/api/runs";
import {
  FIRST_CLASS_BOTS,
  type FirstClassBot,
} from "@/lib/whats-next/firstClassBots";

import { messagesFromEvents } from "./messagesFromEvents";

const whatsNext = FIRST_CLASS_BOTS["whats-next"] as FirstClassBot;

let nextSeq = 1;
function evt(
  type: RunEvent["type"],
  fields: Partial<Omit<RunEvent, "type">> = {},
): RunEvent {
  // The cast re-associates the decomposed type/data pair with the
  // discriminated union — call sites pass a literal type plus the
  // matching payload shape.
  return {
    seq: fields.seq ?? nextSeq++,
    timestamp: fields.timestamp ?? new Date().toISOString(),
    type,
    run_id: "run_test",
    branch_id: fields.branch_id,
    node_id: fields.node_id,
    data: fields.data,
  } as RunEvent;
}

describe("messagesFromEvents (whats-next v2)", () => {
  it("returns no messages when the stream is empty", () => {
    nextSeq = 1;
    const out = messagesFromEvents({
      bot: whatsNext,
      events: [],
      snapshot: null,
    });
    expect(out).toEqual([]);
  });

  it("pushes a running banner on node_started for the nexie agent", () => {
    nextSeq = 1;
    const out = messagesFromEvents({
      bot: whatsNext,
      events: [evt("node_started", { node_id: "nexie" })],
      snapshot: null,
    });
    expect(out).toHaveLength(1);
    expect(out[0]).toMatchObject({
      kind: "banner",
      nodeId: "nexie",
      status: "running",
      label: "Nexie is working",
    });
  });

  it("keeps seed and gate silent", () => {
    nextSeq = 1;
    const out = messagesFromEvents({
      bot: whatsNext,
      events: [
        evt("node_started", { node_id: "seed" }),
        evt("node_finished", { node_id: "seed" }),
        evt("node_started", { node_id: "gate" }),
        evt("node_finished", { node_id: "gate" }),
      ],
      snapshot: null,
    });
    expect(out).toEqual([]);
  });

  it("renders the chat pause with Nexie's reply (instructions) as the prompt, then flips to answered", () => {
    nextSeq = 1;
    const out = messagesFromEvents({
      bot: whatsNext,
      events: [
        evt("node_started", { node_id: "chat" }),
        evt("human_input_requested", {
          node_id: "chat",
          data: {
            interaction_id: "run_test_chat",
            instructions: "Voici mon analyse — je recommande le ticket A.",
            questions: { reply: "…", quick_replies: ["Dispatche A"] },
          },
        }),
        evt("human_answers_recorded", {
          node_id: "chat",
          data: {
            interaction_id: "run_test_chat",
            answers: { message: "ok, dispatche A" },
          },
        }),
      ],
      snapshot: null,
    });
    expect(out).toHaveLength(1);
    expect(out[0]).toMatchObject({
      kind: "human-question",
      nodeId: "chat",
      status: "answered",
      prompt: "Voici mon analyse — je recommande le ticket A.",
      userReply: "ok, dispatche A",
    });
  });

  it("folds assistant_text narration into a bubble, merging consecutive chunks of the same turn", () => {
    nextSeq = 1;
    const out = messagesFromEvents({
      bot: whatsNext,
      events: [
        evt("node_started", { node_id: "nexie" }),
        evt("assistant_text", {
          node_id: "nexie",
          data: { text: "Je regarde le board.", iteration: 0 },
        }),
        evt("assistant_text", {
          node_id: "nexie",
          data: { text: "3 candidats sérieux.", iteration: 0 },
        }),
      ],
      snapshot: null,
    });
    expect(out).toHaveLength(2);
    expect(out[1]).toMatchObject({
      kind: "assistant-text",
      nodeId: "nexie",
      text: "Je regarde le board.\n\n3 candidats sérieux.",
    });
  });

  it("surfaces an ask_user pause on the AGENT node as a human-question keyed by interaction id", () => {
    nextSeq = 1;
    const out = messagesFromEvents({
      bot: whatsNext,
      events: [
        evt("node_started", { node_id: "nexie" }),
        evt("human_input_requested", {
          node_id: "nexie",
          data: {
            interaction_id: "run_test_nexie_ask",
            questions: {
              ask_user_response: "Close these 4 stale tickets?",
              _ask_user_options: [
                { id: "yes", label: "Close all 4" },
                { id: "no", label: "Keep them" },
              ],
              _ask_user_allow_free_text: false,
            },
          },
        }),
        evt("human_answers_recorded", {
          node_id: "nexie",
          data: {
            interaction_id: "run_test_nexie_ask",
            answers: { ask_user_response: "yes" },
          },
        }),
      ],
      snapshot: null,
    });
    // Banner (running) + the ask_user question card.
    const question = out.find((m) => m.kind === "human-question");
    expect(question).toMatchObject({
      kind: "human-question",
      nodeId: "nexie",
      id: "run_test_nexie_ask",
      status: "answered",
      prompt: "Close these 4 stale tickets?",
      userReply: "yes",
    });
  });

  it("dedupes a duplicate node_started (WS replay)", () => {
    nextSeq = 1;
    const out = messagesFromEvents({
      bot: whatsNext,
      events: [
        evt("node_started", { node_id: "nexie" }),
        evt("node_started", { node_id: "nexie" }),
      ],
      snapshot: null,
    });
    expect(out).toHaveLength(1);
  });

  it("renders an unmapped node as ordinary progress instead of hiding it", () => {
    nextSeq = 1;
    const out = messagesFromEvents({
      bot: whatsNext,
      events: [
        evt("node_started", { node_id: "some_other_node" }),
        evt("node_finished", { node_id: "some_other_node" }),
      ],
      snapshot: null,
    });
    expect(out).toHaveLength(1);
    expect(out[0]).toMatchObject({
      kind: "banner",
      nodeId: "some_other_node",
      label: "some_other_node",
      status: "done",
    });
  });
});
