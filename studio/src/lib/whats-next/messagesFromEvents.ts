// Thin wrapper that adapts the whats-next bot's nodeMap to the
// generic runChat folder (`@/lib/runChat/messagesFromEvents`).
//
// The actual fold logic lives in runChat. Here we:
//   1. Build a `whatsNextKindResolver(bot)` that maps the bot's
//      nodeMap entries into the generic NodeKindResolver shape —
//      including the optional `extension()` hook that turns
//      `followCardKind: "roadmap"|"issuesSummary"|"survey"` into
//      typed extension payloads.
//   2. Re-export the result of the generic fold as
//      `WhatsNextMessage[]`. The `postProcess` step on the resolver
//      lifts each `ExtensionMessage` back into the bot's typed
//      cards (RoadmapCardMessage, IssuesSummaryMessage,
//      SurveyCardMessage, PlanHandedOffMessage) so the 8 WhatsNext
//      components don't need to know about the generic seam.

import type { RunEvent, RunSnapshot } from "@/api/runs";
import type {
  ExtensionPayload,
  NodeKindResolver,
} from "@/lib/runChat/nodeKindResolver";
import {
  messagesFromEventsCached as runChatMessagesFromEventsCached,
  type MessagesFoldCache as RunChatMessagesFoldCache,
} from "@/lib/runChat/messagesFromEvents";
import type {
  ExtensionMessage,
  RunChatMessage,
} from "@/lib/runChat/types";

import type { FirstClassBot } from "./firstClassBots";
import {
  asEmitOutput,
  asRoadmapDoc,
  asSurveyOutput,
  type IssuesSummaryMessage,
  type PlanHandedOffMessage,
  type RoadmapCardMessage,
  type SurveyCardMessage,
  type WhatsNextMessage,
} from "./messages";

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

// Build a NodeKindResolver from the bot's nodeMap. The resolver does
// three jobs: (a) tell the folder which nodes are banner/human/silent;
// (b) supply per-node labels, prompts, summaryField extraction, and
// textField-aware answer extraction; (c) emit ExtensionPayloads for
// nodes that ship typed cards (Roadmap/Issues/Survey/PlanHandedOff).
// The postProcess() step then lifts those payloads into the
// WhatsNextMessage shapes the bot's components expect.
function makeResolver(bot: FirstClassBot): NodeKindResolver {
  return {
    kind(nodeId) {
      const entry = bot.nodeMap[nodeId];
      if (!entry) return "silent"; // unmapped nodes never produce messages
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
    // Whats-next routes outputs through typed cards via the extension
    // seam; the generic NodeOutputMessage would double-render them.
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
    humanAnswerExtractor(nodeId, answers) {
      const entry = bot.nodeMap[nodeId];
      if (!entry || entry.kind !== "human") return undefined;
      const textKey = entry.textField;
      const approvedKey = entry.approvedField;
      // Per-node formatter takes precedence: it knows the form
      // shape (ask_continue's action+detail, …) so it can produce
      // a meaningful label when textField alone would render the
      // turn as "(empty reply)" — e.g. an operator who picked
      // dispatch_more without typing a filter still sees
      // "dispatch_more" on their answered turn instead of an
      // erasure.
      let text = "";
      if (entry.formatAnswer && answers) {
        text = entry.formatAnswer(answers).trim();
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
    extension(nodeId, iter, eventOutput) {
      const entry = bot.nodeMap[nodeId];
      if (!entry || !entry.followCardKind || !eventOutput) return null;
      switch (entry.followCardKind) {
        case "roadmap": {
          const roadmap = asRoadmapDoc(eventOutput);
          if (!roadmap) return null;
          return { tag: "roadmap", payload: { nodeId, iteration: iter, roadmap } };
        }
        case "survey": {
          const survey = asSurveyOutput(eventOutput);
          if (!survey) return null;
          return { tag: "survey", payload: { nodeId, ...survey } };
        }
        case "issuesSummary": {
          const emit = asEmitOutput(eventOutput);
          if (!emit) return null;
          // emit_action lands two extension cards back-to-back:
          // (1) the issues-summary card, (2) the "plan handed off"
          // milestone marker. The post-emit triage loop keeps the
          // run alive, so the run-level "finished" marker can't
          // double for the milestone the way it used to.
          const list: ExtensionPayload[] = [
            { tag: "issues-summary", payload: { nodeId, ...emit } },
            {
              tag: "plan-handed-off",
              payload: {
                planPath: emit.planPath,
                createdCount: emit.createdIssues.length,
                summary: emit.summary || undefined,
              },
            },
          ];
          return list;
        }
      }
      return null;
    },
    postProcess(messages) {
      // Lift ExtensionMessage entries back into their typed
      // WhatsNextMessage shapes. Anything else passes through
      // unchanged (banner / human-question / session-closed are
      // shared envelopes already).
      const out: RunChatMessage[] = [];
      for (const m of messages) {
        if (m.kind !== "extension") {
          out.push(m);
          continue;
        }
        const ext = m as ExtensionMessage;
        switch (ext.tag) {
          case "roadmap": {
            const p = ext.payload as {
              nodeId: string;
              iteration: number;
              roadmap: NonNullable<ReturnType<typeof asRoadmapDoc>>;
            };
            out.push({
              kind: "roadmap-card",
              id: ext.id,
              nodeId: p.nodeId,
              iteration: p.iteration,
              roadmap: p.roadmap,
            } satisfies RoadmapCardMessage as unknown as RunChatMessage);
            break;
          }
          case "survey": {
            const p = ext.payload as {
              nodeId: string;
            } & NonNullable<ReturnType<typeof asSurveyOutput>>;
            out.push({
              kind: "survey-card",
              id: ext.id,
              nodeId: p.nodeId,
              summary: p.summary,
              openQuestions: p.openQuestions,
              observations: p.observations,
              toplevelDirs: p.toplevelDirs,
              recentCommits: p.recentCommits,
            } satisfies SurveyCardMessage as unknown as RunChatMessage);
            break;
          }
          case "issues-summary": {
            const p = ext.payload as {
              nodeId: string;
            } & NonNullable<ReturnType<typeof asEmitOutput>>;
            out.push({
              kind: "issues-summary",
              id: ext.id,
              nodeId: p.nodeId,
              createdIssues: p.createdIssues,
              failedIssues: p.failedIssues,
              planPath: p.planPath,
              summary: p.summary,
            } satisfies IssuesSummaryMessage as unknown as RunChatMessage);
            break;
          }
          case "plan-handed-off": {
            const p = ext.payload as {
              planPath: string;
              createdCount: number;
              summary?: string;
            };
            out.push({
              kind: "plan-handed-off",
              id: ext.id,
              planPath: p.planPath,
              createdCount: p.createdCount,
              summary: p.summary,
            } satisfies PlanHandedOffMessage as unknown as RunChatMessage);
            break;
          }
        }
      }
      return out;
    },
  };
}

// Stable per-bot resolver so the runChat fold cache key (which uses
// resolver identity) stays valid across renders. Without the cache,
// every fold would replay the full event stream — fine for short
// runs, observable lag for the post-emit triage loop's accumulating
// chat. WeakMap so the resolver is GC'd with the bot definition.
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
