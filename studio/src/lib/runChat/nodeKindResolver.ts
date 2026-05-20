// NodeKindResolver decides how each node id of a running workflow folds
// into the chat transcript. The generic IR resolver derives kind/label
// from `WireWorkflow.nodes`; bot-specific resolvers (whats-next) wrap
// the IR resolver and layer extension cards on top via the optional
// `extension()` hook.
//
// All event-folder logic in `messagesFromEvents.ts` calls into this
// interface — the folder never inspects bot identity directly.

import type { WireWorkflow } from "@/api/runs";

import type {
  HumanQuestionMessage,
  RunChatMessage,
} from "./types";

export type RunChatNodeKind = "banner" | "human" | "silent";

// Extension payload returned by `resolver.extension()` after a banner
// closes. The fold pushes an `ExtensionMessage` carrying this payload
// verbatim; bot-specific renderers consume it. The runChat module
// itself never inspects `payload`.
export interface ExtensionPayload {
  tag: string; // e.g. "roadmap", "issues-summary", "plan-handed-off"
  payload: unknown;
}

// Optional resolver-level customisation for a `human` turn. Returned
// from `humanRenderHints(nodeId, questions)` so bot resolvers can map
// their node-map entries (whats-next: `prompt`, `actions`, default
// quickActions) into the generic HumanQuestionMessage without the
// folder caring about bot identity.
export interface HumanRenderHints {
  prompt?: string;
  actions?: HumanQuestionMessage["actions"];
  quickActions?: HumanQuestionMessage["quickActions"];
}

export interface NodeKindResolver {
  kind(nodeId: string): RunChatNodeKind;
  // Display label for banners (whats-next pulls this from its
  // `nodeMap[nodeId].label`; the IR resolver falls back to node id).
  label(nodeId: string): string;
  // True when an `agent`/`judge`/`compute` node finished and the
  // generic NodeOutputMessage should be pushed. The whats-next
  // resolver returns false uniformly — it routes outputs through
  // `extension()` into typed cards instead, so we don't double-render.
  emitsOutputCard(nodeId: string): boolean;
  // Optional: bot-specific summary plucked from a finished banner's
  // output (e.g. whats-next's "board_summary"). The IR resolver
  // returns undefined.
  bannerSummary?(nodeId: string, eventOutput: Record<string, unknown> | null): string | undefined;
  // Optional: bot-specific hints for human nodes. The IR resolver
  // returns undefined → renderer uses sensible defaults
  // (prompt = "Reply to continue.", quickActions = ["skip", "idk"]).
  humanRenderHints?(nodeId: string): HumanRenderHints | undefined;
  // Optional: bot-specific extraction of the answered text + the
  // optional `approved` flag from the answers map. The whats-next
  // resolver supplies a textField-aware version (each human node
  // declares which field holds the user-visible text). The IR
  // resolver omits this → folder picks the longest string value
  // and any bool named "approved".
  humanAnswerExtractor?(
    nodeId: string,
    answers: Record<string, unknown> | null,
  ): { text: string; approved?: boolean } | undefined;
  // Optional: extension card emitted after a banner closes. Returns
  // null when no extension applies; the folder skips the push.
  extension?(
    nodeId: string,
    iteration: number,
    eventOutput: Record<string, unknown> | null,
  ): ExtensionPayload | ExtensionPayload[] | null;
  // Optional: post-process the folded message array. The whats-next
  // resolver hooks this to lift `ExtensionMessage`s back into its
  // typed `RoadmapCardMessage | IssuesSummaryMessage | …` shapes.
  postProcess?(messages: RunChatMessage[]): RunChatMessage[];
}

// irKindResolver maps the workflow's IR node kinds to the chat shape.
// agent/judge/tool/compute → banner; human → human (free-text by
// default); router/done/fail → silent (the run-level termination
// events handle their UI). Tool nodes get a banner so the operator
// can see them, but `emitsOutputCard` returns false for tool nodes
// to avoid duplicating the bottom-panel log output for shell commands.
export function irKindResolver(workflow: WireWorkflow | null): NodeKindResolver {
  // Pre-index nodes by id so kind/label lookups are O(1) across the fold.
  // A missing workflow is allowed during the initial load window —
  // every lookup falls back to "banner" so events render *something*
  // (better degradation than dropping all events while we wait for
  // the workflow fetch to land).
  const byId = new Map<string, { kind: string }>();
  if (workflow) {
    for (const n of workflow.nodes) byId.set(n.id, { kind: n.kind });
  }
  return {
    kind(nodeId) {
      const node = byId.get(nodeId);
      if (!node) return "banner";
      switch (node.kind) {
        case "human":
          return "human";
        case "router":
        case "done":
        case "fail":
          return "silent";
        case "agent":
        case "judge":
        case "tool":
        case "compute":
        default:
          return "banner";
      }
    },
    label(nodeId) {
      return nodeId;
    },
    emitsOutputCard(nodeId) {
      const node = byId.get(nodeId);
      if (!node) return false;
      // Tool nodes already stream their stdout/stderr into the bottom
      // log panel; rendering the output as a chat card would duplicate
      // the noise without adding signal. Compute nodes are
      // deterministic projections of upstream data — typically dull
      // by themselves, but useful to surface so the operator sees
      // *something* between agent steps.
      return node.kind === "agent" || node.kind === "judge" || node.kind === "compute";
    },
  };
}
