// messagesFromEvents folds a chronologically-ordered RunEvent[] stream
// into the RunChatMessage[] the chat transcript consumes.
//
// The mapping is driven by a NodeKindResolver: each node id resolves
// to "banner" | "human" | "silent". Events for "silent" nodes are
// dropped (router/done/fail handled by run-level termination events).
// Agent/judge/compute banners can additionally emit a generic
// NodeOutputMessage when `resolver.emitsOutputCard(id)` is true. Bot
// extensions hook in via `resolver.extension(...)` which pushes an
// opaque ExtensionMessage right after the banner closes.
//
// Output ordering: messages are pushed in the order of the originating
// events. Each agent node produces a banner; the banner closes when
// the matching `node_finished` arrives and (a) the generic
// node-output card and (b) any extension cards are pushed right after.
// Human nodes produce a single human-question message that flips from
// "pending" to "answered" when the next `human_answers_recorded` lands.
//
// Iteration: the runtime stamps `iteration` on `node_started.data`.
// Other event types omit it; we recover the value from `nodeIteration`
// (last `node_started` seen for the same node id). Without this
// fallback, the second iteration of a loop body would collide with
// the first under the same key, the dedupe check would drop it, and
// the UI would render the answered first iteration in place of the
// pending second one.

import type { RunEvent, RunSnapshot } from "@/api/runs";

import type { NodeKindResolver } from "./nodeKindResolver";
import type {
  BannerMessage,
  BannerStatus,
  ExtensionMessage,
  HumanQuestionMessage,
  NodeOutputMessage,
  RunChatMessage,
} from "./types";

interface MapInputs {
  resolver: NodeKindResolver;
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
// next snapshot refetch to learn what a node produced.
function extractEventOutput(
  evt: RunEvent,
): Record<string, unknown> | null {
  const data = evt.data;
  if (!data || typeof data !== "object") return null;
  const out = (data as Record<string, unknown>).output;
  if (!out || typeof out !== "object" || Array.isArray(out)) return null;
  return out as Record<string, unknown>;
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
// needs. Exported so callers (the useRunChatMessages hook) can persist
// it across renders and resume folding from a cursor instead of
// replaying the whole event stream every push.
export interface FolderState {
  out: RunChatMessage[];
  bannerIdx: Map<string, number>;
  humanIdx: Map<string, number>;
  activeBannerByNode: Map<string, number>;
  // Latest iteration seen for each node, primed from node_started
  // events (which always carry iteration in their data). Used as a
  // fallback for downstream events whose payload omits the iteration
  // field — notably human_input_requested, which only carries the
  // interaction_id + questions. Without this fallback the second
  // human turn in a revise loop collides with the first (both default
  // to iter 0), the dedupe check drops the new pending turn, and the
  // UI shows the answered first iteration without the form.
  nodeIteration: Map<string, number>;
  latestPendingHumanKey: string | null;
}

export function newFolderState(): FolderState {
  return {
    out: [],
    bannerIdx: new Map(),
    humanIdx: new Map(),
    activeBannerByNode: new Map(),
    nodeIteration: new Map(),
    latestPendingHumanKey: null,
  };
}

// MessagesFoldCache snapshots the folder state plus enough event-stream
// identity to recognise an append (same firstSeq, monotonic lastSeq) vs
// a replay (firstSeq mismatched → must full-refold). Resolver identity
// is part of the key because resolver changes alter how events fold —
// e.g. an IR fetch landing after some events flips silent-by-default
// nodes into typed kinds.
export interface MessagesFoldCache {
  resolver: NodeKindResolver;
  firstSeq: number | null;
  lastSeq: number;
  state: FolderState;
}

export function messagesFromEvents(inputs: MapInputs): RunChatMessage[] {
  return messagesFromEventsCached(inputs, null).messages;
}

// messagesFromEventsCached is the incremental variant. When `prev` is
// compatible (same resolver, same first-event seq), it resumes folding
// from `prev.lastSeq + 1` and mutates `prev.state.out` in place.
// Otherwise it folds from scratch.
//
// Snapshot identity is intentionally NOT part of the cache key: the
// fold reads snapshot only at node_finished time to materialise the
// banner summary; once that summary is baked into `out[idx]`, later
// snapshot updates don't retroactively change it. So a stale-snapshot
// resume produces the same output as a fresh refold would.
export function messagesFromEventsCached(
  inputs: MapInputs,
  prev: MessagesFoldCache | null,
): { messages: RunChatMessage[]; cache: MessagesFoldCache } {
  const { resolver, events, snapshot } = inputs;
  const firstSeq = events.length > 0 ? events[0]?.seq ?? null : null;

  const canResume =
    prev !== null &&
    prev.resolver === resolver &&
    prev.firstSeq !== null &&
    prev.firstSeq === firstSeq;

  let state: FolderState;
  let lastSeq: number;

  if (canResume) {
    state = prev!.state;
    lastSeq = prev!.lastSeq;
  } else {
    state = newFolderState();
    lastSeq = -1;
  }

  // Find the slice of events we haven't processed yet. The store
  // dedupes by seq and appends-only, so events with seq <= lastSeq
  // are stable at the head — binary-search past them. For a 200-event
  // run with one new event per tick this turns an O(N) per-tick scan
  // into O(log N).
  let startIdx = 0;
  if (lastSeq >= 0) {
    let lo = 0;
    let hi = events.length - 1;
    while (lo <= hi) {
      const mid = (lo + hi) >> 1;
      const midSeq = events[mid]?.seq ?? 0;
      if (midSeq <= lastSeq) lo = mid + 1;
      else hi = mid - 1;
    }
    startIdx = lo;
  }
  // Sort the new tail by monotonic seq. The runtime emits events with
  // strictly increasing seq, but replay + live tail can interleave
  // arrivals and a tool_started landing before its parent node_started
  // would otherwise be silently dropped (no banner to attribute to).
  // Sorting only the tail keeps this defensive without paying for the
  // whole array on every tick.
  const newEvents = events.slice(startIdx);
  newEvents.sort((a, b) => (a.seq ?? 0) - (b.seq ?? 0));

  const startLen = state.out.length;
  for (const evt of newEvents) {
    const seq = evt.seq ?? 0;
    if (seq <= lastSeq) continue;
    processEvent(evt, state, resolver, snapshot);
    if (seq > lastSeq) lastSeq = seq;
  }

  // Allow the resolver to post-process the folded output (whats-next
  // uses this to lift ExtensionMessages back into typed cards). Only
  // re-run when new messages were appended this tick — otherwise we'd
  // walk the full O(N) message array on every WS event with nothing
  // to change. Resolvers that don't override postProcess get the
  // messages unchanged.
  let messages: RunChatMessage[] = state.out;
  if (resolver.postProcess && state.out.length > startLen) {
    messages = resolver.postProcess(state.out);
    if (messages !== state.out) {
      state.out = messages;
    }
  }

  return {
    messages,
    cache: {
      resolver,
      firstSeq,
      lastSeq,
      state,
    },
  };
}

function processEvent(
  evt: RunEvent,
  state: FolderState,
  resolver: NodeKindResolver,
  snapshot: RunSnapshot | null,
): void {
  const out = state.out;
  const bannerIdx = state.bannerIdx;
  const humanIdx = state.humanIdx;
  const activeBannerByNode = state.activeBannerByNode;
  const nodeIteration = state.nodeIteration;
  let latestPendingHumanKey = state.latestPendingHumanKey;

  if (!evt.type) return;
  switch (evt.type) {
    case "node_started": {
      const nodeId = evt.node_id;
      if (!nodeId) break;
      const kind = resolver.kind(nodeId);
      if (kind === "silent") break;

      const iter = iterationOf(evt);
      // Record the iteration so later events on this node (notably
      // human_input_requested) can resolve it even when their own
      // payload omits the iteration field — see FolderState comment.
      nodeIteration.set(nodeId, iter);
      if (kind === "banner") {
        const key = bannerId(nodeId, iter);
        if (bannerIdx.has(key)) break; // dedupe replay
        const idx = out.length;
        out.push({
          kind: "banner",
          id: key,
          nodeId,
          label: resolver.label(nodeId),
          status: "running",
        } satisfies BannerMessage);
        bannerIdx.set(key, idx);
        activeBannerByNode.set(nodeId, idx);
      }
      // "human" entries: the human-question message is pushed on the
      // matching `human_input_requested` event because the request
      // carries the resolved `questions` map. node_started alone is
      // enough to know the human node is about to ask, but the
      // form renderer needs the schema/questions to render correctly.
      break;
    }

    case "node_finished": {
      const nodeId = evt.node_id;
      if (!nodeId) break;
      const kind = resolver.kind(nodeId);
      if (kind === "silent") break;

      // node_finished events also omit `iteration` from the payload
      // today — same fallback as human_input_requested. Without it,
      // every node_finished for a re-entered node updates the iter-0
      // banner and the iter-1+ banner stays stuck on "running" with
      // its loading phrase rotating even though the node has long
      // completed.
      const iter = nodeIteration.get(nodeId) ?? iterationOf(evt);
      if (kind !== "banner") break;
      const key = bannerId(nodeId, iter);
      const idx = bannerIdx.get(key);
      if (idx === undefined) break;
      // node_finished embeds the node's output verbatim. Prefer that
      // over checkpointOutput(snapshot, ...) because the snapshot may
      // not have been refreshed yet under live tail — the WS pushes
      // events as they happen, while a fresh snapshot only lands on
      // the next periodic refetch. Reading from the event lets the
      // follow-up node-output/extension cards materialise the moment
      // the node completes instead of after a navigation away-and-
      // back triggers a full refold against the now-current snapshot.
      // Falls back to the snapshot path for any (legacy) event missing
      // the embedded output.
      const eventOutput = extractEventOutput(evt) ?? checkpointOutput(snapshot, nodeId);
      const summary = resolver.bannerSummary
        ? resolver.bannerSummary(nodeId, eventOutput)
        : undefined;
      const updated: BannerMessage = {
        ...(out[idx] as BannerMessage),
        status: "done",
        summary: summary || undefined,
        // Drop the live progress once the banner is done — the
        // summary (when present) takes its place.
        progress: undefined,
      };
      out[idx] = updated;
      activeBannerByNode.delete(nodeId);

      // Generic node-output card. The resolver decides per node
      // whether it makes sense (e.g. tool nodes opt out — their
      // stdout already lives in the bottom log panel).
      if (eventOutput && resolver.emitsOutputCard(nodeId)) {
        out.push({
          kind: "node-output",
          id: `${nodeId}:${iter}:output`,
          nodeId,
          iteration: iter,
          output: eventOutput,
        } satisfies NodeOutputMessage);
      }

      // Bot-specific extension cards (whats-next uses this for
      // Roadmap/Survey/IssuesSummary/PlanHandedOff).
      if (resolver.extension) {
        const ext = resolver.extension(nodeId, iter, eventOutput);
        if (ext) {
          const list = Array.isArray(ext) ? ext : [ext];
          for (let i = 0; i < list.length; i++) {
            const e = list[i]!;
            out.push({
              kind: "extension",
              id: `${nodeId}:${iter}:ext:${i}:${e.tag}`,
              tag: e.tag,
              payload: e.payload,
            } satisfies ExtensionMessage);
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
      const kind = resolver.kind(nodeId);
      if (kind !== "human") break;

      // The runtime omits `iteration` from human_input_requested
      // event payloads today — only node_started carries it. Without
      // the fallback, the second human turn of a revise loop shares
      // the iter-0 key with the first, the dedupe check below drops
      // it, and the user sees the answered iter-0 bubble (no form)
      // instead of the new pending iter-1 form.
      const iter = nodeIteration.get(nodeId) ?? iterationOf(evt);
      const key = humanId(nodeId, iter);
      if (humanIdx.has(key)) break; // dedupe replay
      // Pull through the runtime-supplied questions payload (set when
      // the engine resolved field definitions from the workflow's
      // human node or from an LLM-fill step). The form renderer uses
      // it to label inputs, hint at allowed values, etc.
      const questions =
        evt.data?.questions && typeof evt.data.questions === "object"
          ? (evt.data.questions as Record<string, unknown>)
          : undefined;
      const hints = resolver.humanRenderHints?.(nodeId);
      const idx = out.length;
      out.push({
        kind: "human-question",
        id: key,
        nodeId,
        prompt: hints?.prompt ?? "Reply to continue.",
        status: "pending",
        actions: hints?.actions,
        quickActions: hints?.quickActions,
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
      // silent intermediate loop nodes). Fallback to the latest
      // pending key when an older event payload arrives without a
      // node_id — the runtime always stamps node_id today, but the
      // older format may still surface in replay.
      let nodeId = evt.node_id;
      let key: string;
      if (nodeId) {
        // Same iteration-fallback rationale as human_input_requested:
        // the runtime omits `iteration` from this event payload, so
        // we read it from the most recent node_started.
        const iter = nodeIteration.get(nodeId) ?? iterationOf(evt);
        key = humanId(nodeId, iter);
      } else if (latestPendingHumanKey) {
        key = latestPendingHumanKey;
        const fallbackEntry = humanIdx.get(key);
        if (fallbackEntry === undefined) break;
        const pending = out[fallbackEntry] as HumanQuestionMessage | undefined;
        nodeId = pending?.nodeId;
      } else {
        break;
      }
      if (!nodeId) break;
      const kind = resolver.kind(nodeId);
      if (kind !== "human") break;
      const idx = humanIdx.get(key);
      if (idx === undefined) break;
      const current = out[idx] as HumanQuestionMessage;
      const answers = (evt.data?.answers ?? null) as Record<string, unknown> | null;
      // Extraction strategy: resolver-supplied override wins (whats-next
      // uses bot-declared textField/approvedField), else fall back to
      // a generic "longest string + approved bool" pass. Both are
      // best-effort — answered turns survive with `userReply: ""` if
      // no string lands.
      let text = "";
      let approved: boolean | undefined;
      const overridden = resolver.humanAnswerExtractor?.(nodeId, answers);
      if (overridden) {
        text = overridden.text;
        approved = overridden.approved;
      } else if (answers) {
        for (const [k, v] of Object.entries(answers)) {
          if (typeof v === "string" && v.length > text.length) {
            text = v;
          } else if (typeof v === "boolean" && k === "approved") {
            approved = v;
          }
        }
      }
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
      // No active banners expected here (every node should have fired
      // node_finished first), but if one is still flagged running we
      // coerce it to done — a perpetual spinner under a "session
      // finished" footer looks broken.
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
      finalizeActiveBanners(
        out,
        activeBannerByNode,
        "failed",
        asString(evt.data?.error),
      );
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
  out: RunChatMessage[],
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
