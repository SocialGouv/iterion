// Thin wrapper that adapts the whats-next bot's nodeMap to the
// generic runChat folder (`@/lib/runChat/messagesFromEvents`).
//
// The actual fold logic lives in runChat. Here we build a
// `whatsNextKindResolver(bot)` that maps the bot's nodeMap entries
// into the generic NodeKindResolver shape (kind / label /
// summaryField / human hints / textField-aware answer extraction).
//
// whats-next v2 note: the extension-card machinery (roadmap / survey /
// issues-summary / dispatch-candidates / triage-summary typed cards +
// the postProcess lift) is gone with the v1 form state machine — Nexie
// speaks markdown through assistant_text narration and the chat
// node's instructions, so the generic message shapes cover the whole
// transcript.

import type { RunEvent, RunSnapshot } from "@/api/runs";
import type { NodeKindResolver } from "@/lib/runChat/nodeKindResolver";
import {
  messagesFromEventsCached as runChatMessagesFromEventsCached,
  type MessagesFoldCache as RunChatMessagesFoldCache,
} from "@/lib/runChat/messagesFromEvents";

import type { FirstClassBot } from "./firstClassBots";
import type { WhatsNextMessage } from "./messages";

interface MapInputs {
  bot: FirstClassBot;
  events: ReadonlyArray<RunEvent>;
  snapshot: RunSnapshot | null;
}

export interface MessagesFoldCache {
  bot: FirstClassBot;
  // The underlying runChat cache reuses the resolver identity for
  // its incremental cache key. We keep the cache alive across calls
  // by holding a stable resolver reference per bot — see
  // `resolverForBot` below.
  inner: RunChatMessagesFoldCache;
}

// Build a NodeKindResolver from the bot's nodeMap: (a) tell the folder
// which nodes are banner/human/silent; (b) supply per-node labels,
// prompts, summaryField extraction, and textField-aware answer
// extraction.
function makeResolver(bot: FirstClassBot): NodeKindResolver {
  return {
    kind(nodeId) {
      const entry = bot.nodeMap[nodeId];
      // The manifest contract says an unmapped node degrades to an ordinary
      // progress event. Banner is that representation in the chat fold: it
      // keeps a newly-added tool/judge visible until the bundle gives it a
      // deliberate label or marks it silent. Hiding it made the supposedly
      // safe default fail closed and left long reviewer calls looking stuck.
      if (!entry) return "banner";
      switch (entry.kind) {
        case "banner":
          return "banner";
        case "human":
          return "human";
        case "silent":
        default:
          return "silent";
      }
    },
    label(nodeId) {
      return bot.nodeMap[nodeId]?.label ?? nodeId;
    },
    // Nexie's turn output is plumbing (reply/close/quick_replies land
    // on the chat turn via instructions; dispatched_ids feeds the
    // watch list) — a generic NodeOutputMessage would double-render
    // the reply as a JSON card.
    emitsOutputCard() {
      return false;
    },
    bannerSummary(nodeId, eventOutput) {
      const entry = bot.nodeMap[nodeId];
      if (!entry || !entry.summaryField || !eventOutput) return undefined;
      const v = eventOutput[entry.summaryField];
      return typeof v === "string" ? v : undefined;
    },
    humanRenderHints(nodeId) {
      const entry = bot.nodeMap[nodeId];
      if (!entry || entry.kind !== "human") return undefined;
      return {
        prompt: entry.prompt,
        actions: entry.actions,
      };
    },
    humanAnswerExtractor(nodeId, answers, upstream) {
      const entry = bot.nodeMap[nodeId];
      if (!entry || entry.kind !== "human") return undefined;
      const textKey = entry.textField;
      const approvedKey = entry.approvedField;
      let text = "";
      if (entry.formatAnswer && answers) {
        text = entry.formatAnswer(answers, upstream).trim();
      }
      if (!text) {
        text =
          textKey && answers && typeof answers[textKey] === "string"
            ? (answers[textKey] as string)
            : "";
      }
      const approved =
        approvedKey && answers && typeof answers[approvedKey] === "boolean"
          ? (answers[approvedKey] as boolean)
          : undefined;
      return { text, approved };
    },
  };
}

// Stable per-bot resolver so the runChat fold cache key (which uses
// resolver identity) stays valid across renders. Without the cache,
// every fold would replay the full event stream — fine for short
// runs, observable lag for a long chat session. WeakMap so the
// resolver is GC'd with the bot definition.
const resolverByBot = new WeakMap<FirstClassBot, NodeKindResolver>();
function resolverForBot(bot: FirstClassBot): NodeKindResolver {
  let r = resolverByBot.get(bot);
  if (!r) {
    r = makeResolver(bot);
    resolverByBot.set(bot, r);
  }
  return r;
}

export function messagesFromEvents(inputs: MapInputs): WhatsNextMessage[] {
  return messagesFromEventsCached(inputs, null).messages;
}

export function messagesFromEventsCached(
  inputs: MapInputs,
  prev: MessagesFoldCache | null,
): { messages: WhatsNextMessage[]; cache: MessagesFoldCache } {
  const resolver = resolverForBot(inputs.bot);
  const { messages, cache } = runChatMessagesFromEventsCached(
    {
      resolver,
      events: inputs.events,
      snapshot: inputs.snapshot,
    },
    prev?.bot === inputs.bot ? prev.inner : null,
  );
  return {
    messages: messages as WhatsNextMessage[],
    cache: { bot: inputs.bot, inner: cache },
  };
}
