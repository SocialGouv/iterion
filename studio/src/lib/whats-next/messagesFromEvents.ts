// messagesFromEvents folds a chronologically-ordered RunEvent[] stream
// into the WhatsNextMessage[] the chat transcript consumes.
//
// The mapping is driven by the bot's `nodeMap`: each known node id has
// a `kind` ("banner" | "human" | "silent" | …) and optional rules
// (summaryField, followCardKind, prompt, actions). Events for unknown
// nodes are silently dropped — keeps the chat focused on the steps
// the bot author chose to surface.
//
// Output ordering: messages are pushed in the order of the originating
// events. Each agent node produces a banner; the banner closes when
// the matching `node_finished` arrives and (for `followCardKind`) a
// typed card is pushed right after. Human nodes produce a single
// human-question message that flips from "pending" to "answered" when
// the next `run_resumed` lands.
//
// Iteration: the runtime stamps `iteration` on `node_started.data`.
// For nodes inside `approval_loop(10)` (revise_roadmap, human_review,
// carry_roadmap), this lets us key one message per iteration so the
// transcript shows the loop progression instead of mutating a single
// entry.

import type { RunEvent, RunSnapshot } from "@/api/runs";
import type { FirstClassBot } from "@/lib/whats-next/firstClassBots";

import {
  asEmitOutput,
  asRoadmapDoc,
  asSurveyOutput,
  type WhatsNextMessage,
  type BannerMessage,
  type BannerStatus,
  type HumanQuestionMessage,
  type RoadmapCardMessage,
  type IssuesSummaryMessage,
  type SurveyCardMessage,
} from "./messages";

interface MapInputs {
  bot: FirstClassBot;
  events: ReadonlyArray<RunEvent>;
  snapshot: RunSnapshot | null;
}

// Generic accessor for snapshot.run.checkpoint.outputs[nodeId]. Used
// to pull the structured agent output keyed by node id. The runtime
// embeds the same outputs map in artifact_written events, but the
// checkpoint is the source of truth and survives WS reconnects.
function checkpointOutput(
  snapshot: RunSnapshot | null,
  nodeId: string,
): Record<string, unknown> | null {
  const checkpoint = (snapshot?.run.checkpoint ?? null) as
    | { outputs?: Record<string, Record<string, unknown>> }
    | null;
  return checkpoint?.outputs?.[nodeId] ?? null;
}

// Pulls the structured `output` map out of a node_finished event. The
// runtime stamps the full node output verbatim onto data.output as
// part of the event payload, so consumers don't have to wait for the
// next snapshot refetch to learn what a node produced. Returns null
// when the event predates this convention or the output is not an
// object (defensive against arbitrary tool plugins).
function extractEventOutput(
  evt: RunEvent,
): Record<string, unknown> | null {
  const data = evt.data;
  if (!data || typeof data !== "object") return null;
  const out = (data as Record<string, unknown>).output;
  if (!out || typeof out !== "object" || Array.isArray(out)) return null;
  return out as Record<string, unknown>;
}

function getString(obj: Record<string, unknown> | null, key: string): string {
  const v = obj?.[key];
  return typeof v === "string" ? v : "";
}

// pickToolHint extracts a short human-readable hint from a tool_started
// event's data. The runtime's tool data shape varies by tool; we try
// a handful of well-known fields and fall back to undefined when none
// fits. Defensive against arbitrary tool plugins — never throws.
function pickToolHint(
  data: Record<string, unknown>,
  toolName: string | undefined,
): string | undefined {
  const input = (data.input ?? data.arguments ?? null) as
    | Record<string, unknown>
    | string
    | null;
  if (typeof input === "string") {
    // Some tools serialise input as a JSON string; try to parse and
    // recurse, otherwise treat the whole string as the hint.
    try {
      const parsed = JSON.parse(input) as Record<string, unknown>;
      return pickToolHintFromObject(parsed, toolName);
    } catch {
      return truncateHint(input);
    }
  }
  if (input && typeof input === "object") {
    return pickToolHintFromObject(input as Record<string, unknown>, toolName);
  }
  return undefined;
}

function pickToolHintFromObject(
  input: Record<string, unknown>,
  toolName: string | undefined,
): string | undefined {
  // Common single-argument tools — most informative field first.
  const priorityKeys = [
    "command", // bash
    "file_path", // read_file / write_file
    "path",
    "pattern", // glob / grep
    "query",
    "url",
    "title", // create_issue
    "id", // get_issue / transition_issue
  ];
  for (const key of priorityKeys) {
    const v = input[key];
    if (typeof v === "string" && v.length > 0) {
      return truncateHint(v);
    }
  }
  // Last resort: the first string field, if any.
  for (const v of Object.values(input)) {
    if (typeof v === "string" && v.length > 0) {
      return truncateHint(v);
    }
  }
  return toolName;
}

function truncateHint(s: string): string {
  const flat = s.replace(/\s+/g, " ").trim();
  if (flat.length <= 60) return flat;
  return flat.slice(0, 57) + "…";
}

function iterationOf(evt: RunEvent): number {
  const raw = evt.data?.iteration;
  return typeof raw === "number" ? raw : 0;
}

function bannerId(nodeId: string, iter: number) {
  return `${nodeId}:${iter}`;
}

function humanId(nodeId: string, iter: number) {
  return `${nodeId}:${iter}:question`;
}

// FolderState carries the mutable bookkeeping the per-event handler
// needs. Exported so callers (the WhatsNext hook) can persist it across
// renders and resume folding from a cursor instead of replaying the
// whole event stream every push.
export interface FolderState {
  out: WhatsNextMessage[];
  bannerIdx: Map<string, number>;
  humanIdx: Map<string, number>;
  activeBannerByNode: Map<string, number>;
  latestPendingHumanKey: string | null;
}

export function newFolderState(): FolderState {
  return {
    out: [],
    bannerIdx: new Map(),
    humanIdx: new Map(),
    activeBannerByNode: new Map(),
    latestPendingHumanKey: null,
  };
}

// MessagesFoldCache snapshots the folder state plus enough event-stream
// identity to recognise an append (same firstSeq, monotonic lastSeq) vs
// a replay (firstSeq mismatched → must full-refold). Bot identity is
// part of the key because nodeMap changes alter how events fold.
export interface MessagesFoldCache {
  bot: FirstClassBot;
  firstSeq: number | null;
  lastSeq: number;
  state: FolderState;
}

export function messagesFromEvents(inputs: MapInputs): WhatsNextMessage[] {
  return messagesFromEventsCached(inputs, null).messages;
}

// messagesFromEventsCached is the incremental variant. When `prev` is
// compatible (same bot, same first-event seq), it resumes folding from
// `prev.lastSeq + 1` and mutates `prev.state.out` in place. Otherwise
// it folds from scratch.
//
// Snapshot identity is intentionally NOT part of the cache key: the
// fold reads snapshot only at node_finished time to materialise the
// banner summary; once that summary is baked into `out[idx]`, later
// snapshot updates don't retroactively change it. So a stale-snapshot
// resume produces the same output as a fresh refold would.
export function messagesFromEventsCached(
  inputs: MapInputs,
  prev: MessagesFoldCache | null,
): { messages: WhatsNextMessage[]; cache: MessagesFoldCache } {
  const { bot, events, snapshot } = inputs;
  const firstSeq = events.length > 0 ? events[0]?.seq ?? null : null;

  const canResume =
    prev !== null &&
    prev.bot === bot &&
    prev.firstSeq !== null &&
    prev.firstSeq === firstSeq;

  let state: FolderState;
  let lastSeq: number;
  let toProcess: ReadonlyArray<RunEvent>;

  if (canResume) {
    state = prev!.state;
    lastSeq = prev!.lastSeq;
    toProcess = events;
  } else {
    state = newFolderState();
    lastSeq = -1;
    toProcess = events;
  }

  // Sort by monotonic seq before folding. The runtime emits events
  // with strictly increasing seq, but the store's applyEventsBatch
  // dedupes-by-seq without re-sorting, and replay + live tail can
  // interleave their respective arrivals. A tool_started landing
  // before its parent node_started would otherwise be silently
  // dropped (no banner to attribute to). Stable sort: equal seq —
  // shouldn't happen but tolerate — keeps the original order.
  const sortedEvents = toProcess
    .slice()
    .sort((a, b) => (a.seq ?? 0) - (b.seq ?? 0));

  for (const evt of sortedEvents) {
    const seq = evt.seq ?? 0;
    if (seq <= lastSeq) continue;
    processEvent(evt, state, bot, snapshot);
    if (seq > lastSeq) lastSeq = seq;
  }

  return {
    messages: state.out,
    cache: {
      bot,
      firstSeq,
      lastSeq,
      state,
    },
  };
}

function processEvent(
  evt: RunEvent,
  state: FolderState,
  bot: FirstClassBot,
  snapshot: RunSnapshot | null,
): void {
  const out = state.out;
  const bannerIdx = state.bannerIdx;
  const humanIdx = state.humanIdx;
  const activeBannerByNode = state.activeBannerByNode;
  let latestPendingHumanKey = state.latestPendingHumanKey;

  if (!evt.type) return;
  switch (evt.type) {
      case "node_started": {
        const nodeId = evt.node_id;
        if (!nodeId) break;
        const entry = bot.nodeMap[nodeId];
        if (!entry) break;

        const iter = iterationOf(evt);
        if (entry.kind === "banner") {
          const key = bannerId(nodeId, iter);
          if (bannerIdx.has(key)) break; // dedupe replay
          const idx = out.length;
          out.push({
            kind: "banner",
            id: key,
            nodeId,
            label: entry.label ?? nodeId,
            status: "running",
          } satisfies BannerMessage);
          bannerIdx.set(key, idx);
          activeBannerByNode.set(nodeId, idx);
        } else if (entry.kind === "human") {
          // We push a *pending* human question on the matching
          // `human_input_requested`, not on node_started — the request
          // event carries the resolved `questions` map. node_started
          // alone is enough to know the human node is *about* to ask,
          // but Step 3 needs the schema/questions to render the form
          // correctly. So: nothing to do here for human entries.
        }
        // "silent" and other kinds: ignored.
        break;
      }

      case "node_finished": {
        const nodeId = evt.node_id;
        if (!nodeId) break;
        const entry = bot.nodeMap[nodeId];
        if (!entry) break;

        const iter = iterationOf(evt);
        if (entry.kind === "banner") {
          const key = bannerId(nodeId, iter);
          const idx = bannerIdx.get(key);
          if (idx === undefined) break;
          // node_finished embeds the node's output verbatim. Prefer
          // that over checkpointOutput(snapshot, ...) because the
          // snapshot may not have been refreshed yet under live tail
          // — the WS pushes events as they happen, while a fresh
          // snapshot only lands on the next periodic refetch. Reading
          // from the event lets the follow-up survey/roadmap/issues
          // cards materialise the moment the node completes instead
          // of after a navigation away-and-back triggers a full
          // refold against the now-current snapshot. Falls back to
          // the snapshot path for any (legacy) event missing the
          // embedded output.
          const eventOutput = extractEventOutput(evt) ??
            checkpointOutput(snapshot, nodeId);
          const summary = entry.summaryField
            ? getString(eventOutput, entry.summaryField)
            : "";
          const updated: BannerMessage = {
            ...(out[idx] as BannerMessage),
            status: "done",
            summary: summary || undefined,
            // Drop the live progress once the banner is done — the
            // summary takes its place.
            progress: undefined,
          };
          out[idx] = updated;
          activeBannerByNode.delete(nodeId);

          // Post-banner follow-up cards (roadmap, issues-summary).
          if (entry.followCardKind === "roadmap") {
            const roadmap = asRoadmapDoc(eventOutput);
            if (roadmap) {
              out.push({
                kind: "roadmap-card",
                id: `${nodeId}:${iter}:roadmap`,
                nodeId,
                iteration: iter,
                roadmap,
              } satisfies RoadmapCardMessage);
            }
          } else if (entry.followCardKind === "issuesSummary") {
            const emit = asEmitOutput(eventOutput);
            if (emit) {
              out.push({
                kind: "issues-summary",
                id: `${nodeId}:${iter}:issues`,
                nodeId,
                createdIssues: emit.createdIssues,
                failedIssues: emit.failedIssues,
                planPath: emit.planPath,
                summary: emit.summary,
              } satisfies IssuesSummaryMessage);
            }
          } else if (entry.followCardKind === "survey") {
            const survey = asSurveyOutput(eventOutput);
            if (survey) {
              out.push({
                kind: "survey-card",
                id: `${nodeId}:${iter}:survey`,
                nodeId,
                summary: survey.summary,
                openQuestions: survey.openQuestions,
                observations: survey.observations,
                toplevelDirs: survey.toplevelDirs,
                recentCommits: survey.recentCommits,
              } satisfies SurveyCardMessage);
            }
          }
        }
        break;
      }

      case "tool_started": {
        const nodeId = evt.node_id;
        if (!nodeId) break;
        const idx = activeBannerByNode.get(nodeId);
        if (idx === undefined) break;
        const banner = out[idx] as BannerMessage;
        const data = evt.data ?? {};
        const toolName =
          typeof data.tool === "string" && data.tool ? data.tool : undefined;
        const prev = banner.progress ?? { toolCount: 0 };
        out[idx] = {
          ...banner,
          progress: {
            toolCount: prev.toolCount + 1,
            latestTool: toolName ?? prev.latestTool,
            latestToolHint: pickToolHint(data, toolName) ?? prev.latestToolHint,
          },
        };
        break;
      }

      case "human_input_requested": {
        const nodeId = evt.node_id;
        if (!nodeId) break;
        const entry = bot.nodeMap[nodeId];
        if (!entry || entry.kind !== "human") break;

        const iter = iterationOf(evt);
        const key = humanId(nodeId, iter);
        if (humanIdx.has(key)) break; // dedupe replay
        // Pull through the runtime-supplied questions payload (set when
        // the engine resolved field definitions from the workflow's
        // human node or from an LLM-fill step). The form renderer uses
        // it to label inputs, hint at allowed values, etc. — without
        // this the LLM-fill output was discarded and the form fell
        // back to the bot's static prompt.
        const questions =
          evt.data?.questions && typeof evt.data.questions === "object"
            ? (evt.data.questions as Record<string, unknown>)
            : undefined;
        const idx = out.length;
        out.push({
          kind: "human-question",
          id: key,
          nodeId,
          prompt: entry.prompt ?? "Reply to continue.",
          status: "pending",
          actions: entry.actions,
          questions,
        } satisfies HumanQuestionMessage);
        humanIdx.set(key, idx);
        latestPendingHumanKey = key;
        break;
      }

      case "human_answers_recorded": {
        // The runtime stamps the user's answers on the human node.
        // Match the answered turn by node_id (more reliable than
        // following a "latestPending" cursor, which gets confused by
        // the carry_roadmap silent loop). Pull the user-visible text
        // out of the node's nodeMap entry — different human nodes use
        // different schema field names (ask_priorities → context;
        // human_review → feedback).
        //
        // Fallback: when an older event payload arrives without a
        // node_id, use the most recent pending-human key as a last
        // resort. The runtime always stamps node_id today; this kept
        // the tolerance the previous TODO comment promised but never
        // actually wired.
        let nodeId = evt.node_id;
        let key: string;
        if (nodeId) {
          const iter = iterationOf(evt);
          key = humanId(nodeId, iter);
        } else if (latestPendingHumanKey) {
          key = latestPendingHumanKey;
          const fallbackEntry = humanIdx.get(key);
          if (fallbackEntry === undefined) break;
          // Recover the nodeId from the pending message so downstream
          // logic that needs the nodeMap lookup still works.
          const pending = out[fallbackEntry] as HumanQuestionMessage | undefined;
          nodeId = pending?.nodeId;
        } else {
          break;
        }
        if (!nodeId) break;
        const entry = bot.nodeMap[nodeId];
        if (!entry || entry.kind !== "human") break;
        const idx = humanIdx.get(key);
        if (idx === undefined) break;
        const current = out[idx] as HumanQuestionMessage;
        const answers = (evt.data?.answers ?? null) as Record<string, unknown> | null;
        const textKey = entry.textField;
        const approvedKey = entry.approvedField;
        const text =
          (textKey && typeof answers?.[textKey] === "string"
            ? (answers[textKey] as string)
            : "") || "";
        const approved =
          approvedKey && typeof answers?.[approvedKey] === "boolean"
            ? (answers[approvedKey] as boolean)
            : undefined;
        out[idx] = {
          ...current,
          status: "answered",
          userReply: text || current.userReply,
          outcome: approved !== undefined ? { approved } : current.outcome,
        };
        if (latestPendingHumanKey === key) latestPendingHumanKey = null;
        break;
      }

      case "run_finished":
        // No active banners expected here (every node should have
        // fired node_finished first), but if one is still flagged
        // running we coerce it to done — a perpetual spinner under a
        // "session finished" footer looks broken.
        finalizeActiveBanners(out, activeBannerByNode, "done");
        out.push({
          kind: "session-closed",
          id: `closed:${evt.seq}`,
          reason: "finished",
        });
        break;
      case "run_failed":
        // The runtime emits run_failed without a node_failed companion
        // for the in-flight node (no node_failed event type exists),
        // so any active banner stayed at status:"running" forever —
        // appearing as a perpetual spinner alongside the "session
        // failed" footer. Coerce them to failed and surface the run
        // error message if the event carried one.
        finalizeActiveBanners(out, activeBannerByNode, "failed", asString(evt.data?.error));
        out.push({
          kind: "session-closed",
          id: `closed:${evt.seq}`,
          reason: "failed",
        });
        break;
      case "run_cancelled":
        finalizeActiveBanners(out, activeBannerByNode, "failed");
        out.push({
          kind: "session-closed",
          id: `closed:${evt.seq}`,
          reason: "cancelled",
        });
        break;

    default:
      break;
  }
  state.latestPendingHumanKey = latestPendingHumanKey;
}

// finalizeActiveBanners coerces every banner still flagged as running
// (i.e. its node never emitted node_finished) to a terminal status.
// Used on run_finished / run_failed / run_cancelled so the chat
// doesn't end with a spinning banner under the session-closed marker.
// The runtime has no node_failed event type, so the only signal that
// a node in flight is dead is the run-level termination event.
function finalizeActiveBanners(
  out: WhatsNextMessage[],
  active: Map<string, number>,
  status: BannerStatus,
  errorMessage?: string,
): void {
  for (const idx of active.values()) {
    const b = out[idx];
    if (!b || b.kind !== "banner") continue;
    const updated: BannerMessage = {
      ...b,
      status,
      progress: undefined,
    };
    if (status === "failed" && errorMessage) {
      updated.errorMessage = errorMessage;
    }
    out[idx] = updated;
  }
  active.clear();
}

function asString(v: unknown): string | undefined {
  return typeof v === "string" && v !== "" ? v : undefined;
}
