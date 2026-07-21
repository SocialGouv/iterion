// Shared message types for the generic run-chat timeline.
//
// The runChat module folds a RunEvent[] stream into a sequence of
// WhatsNextMessage-shaped entries that any run can render as a chat
// transcript: banners for agent/judge/tool/compute nodes, human-question
// cards for pause-and-resume turns, markdown node-output cards after a
// node finishes, session-closed markers when the run ends.
//
// The whats-next bot extends this shape with bot-specific cards
// (Roadmap, Survey, Issues, PlanHandedOff). Those flow through the
// ExtensionMessage seam: the fold pushes an ExtensionMessage carrying
// an opaque payload, and the bot-specific renderer post-processes it
// back into its typed card shapes. See:
//   - studio/src/lib/whats-next/messages.ts (re-exports + bot types)
//   - studio/src/lib/whats-next/messagesFromEvents.ts (post-processor)

export type BannerStatus = "running" | "done" | "failed";

// Live progress accumulated while a banner is "running". Drives the
// "12 tools used · latest: bash" line under the spinner so a long
// agent step doesn't look frozen during the 30–60s the agent actually
// needs.
export interface BannerProgress {
  // Number of `tool_started` events seen on this node's active iter.
  toolCount: number;
  // Name of the most recent tool the node invoked (e.g. "bash",
  // "read_file"). Used as a "doing X" hint.
  latestTool?: string;
  // Brief textual hint extracted from the latest tool's input — the
  // first argument-ish field, when present. Surfaces "read_file: README.md"
  // rather than just "read_file".
  latestToolHint?: string;
  // Number of `node_recovery` events seen on this node's active iter
  // — i.e. transient delegate failures the engine recovered from
  // (LLM rate limit, http2 endpoint hiccup, …). The card surfaces
  // this as "↻ N retries" so the operator knows a "still running"
  // banner that's actually stuck in retry loops doesn't look frozen.
  retryCount?: number;
  // Short, human-readable summary of the latest delegate_error /
  // node_recovery error. Truncated to the head so a stack-trace
  // dump from claw doesn't blow up the chip width.
  latestRetryError?: string;
}

export interface BannerMessage {
  kind: "banner";
  id: string; // stable: "<nodeId>:<iteration>" (iteration so loop re-entry doesn't collide)
  nodeId: string;
  label: string;
  status: BannerStatus;
  // When status === "done", the optional one-line summary plucked
  // from the node output. Bot resolvers can set summaryField; the
  // generic IR resolver leaves it empty.
  summary?: string;
  errorMessage?: string;
  // Live counters updated while status === "running".
  progress?: BannerProgress;
}

// Quick-action markers the operator can emit instead of typing a
// reply. The bot's prompt (or the agent loop) is expected to recognise
// these tokens. "later" is meaningful only inside loops where the
// flow can re-ask; outside it's effectively a "skip".
export type QuickActionKind = "skip" | "idk" | "later";

// One turn in a review-gate (interaction: review) companion↔human dialogue.
export interface ReviewTurn {
  role: "companion" | "human";
  content?: string;
  verdict?: Record<string, unknown>;
  at?: string;
}

// Metadata for a guided review-&-merge gate, carried on the paused
// human_input_requested event (evt.data.review === true). Drives the
// ReviewMergeCard: the dialogue thread + the squash-merge controls.
export interface ReviewGateMeta {
  turns: ReadonlyArray<ReviewTurn>;
  posture: string; // "human_required" | "agent_verdict_ok"
  mergeStrategy: string; // "squash" | "merge"
  mergeInto: string; // "current" | "none" | <branch>
  maxTurns: number;
  reviewUrl?: string;
  verdict?: Record<string, unknown>;
}

export interface HumanQuestionMessage {
  kind: "human-question";
  id: string;
  nodeId: string;
  prompt: string;
  // Pending = awaiting user; answered = past turn in the loop.
  status: "pending" | "answered";
  // The verbatim text the user typed when status === "answered".
  userReply?: string;
  // Optional structured outcome (e.g. { approved: true }).
  outcome?: Record<string, unknown>;
  // For typed-review nodes: which action buttons to render
  // (approve / request_revision). Bot-specific resolvers set this
  // from the node's output schema. The generic IR resolver leaves
  // it undefined; the renderer falls back to a free-text submit.
  actions?: ReadonlyArray<"approve" | "request_revision">;
  // Runtime-resolved questions payload (the same data the engine
  // writes into checkpoint.InteractionQuestions). Carries field
  // schema / labels / hints — the form renderer uses these.
  questions?: Record<string, unknown>;
  // Inline shortcuts the operator can pick instead of typing a
  // reply. Only meaningful on free-text turns (the form path
  // already gives the operator structured choices). Default for
  // free-text turns: ["skip", "idk"].
  quickActions?: ReadonlyArray<QuickActionKind>;
  // Set when this pause is a guided review-&-merge gate
  // (interaction: review). The renderer shows ReviewMergeCard — the
  // companion dialogue + squash-merge controls — instead of the
  // free-text / schema form.
  review?: ReviewGateMeta;
  // Async question (ADR-081, ask_user_async): the run KEEPS EXECUTING
  // while this is pending. The renderer shows a non-blocking answer
  // card posting to /interactions/{id}/answer instead of the pause
  // form; interaction_answered flips it.
  async?: boolean;
  // The interaction record ID (always set when async).
  interactionId?: string;
}

// Generic node-output card pushed after an agent/judge/compute node
// finishes. The renderer pretty-prints the structured output as
// markdown (single string field → render as-is; multi-field → headed
// sections; objects/arrays → fenced JSON). See NodeOutputCard.tsx.
export interface NodeOutputMessage {
  kind: "node-output";
  id: string; // "<nodeId>:<iteration>:output"
  nodeId: string;
  iteration: number;
  output: Record<string, unknown>;
}

export interface SessionClosedMessage {
  kind: "session-closed";
  id: string;
  // "finished" — run reached a Done terminal node.
  // "failed" — run hit Fail or unrecoverable error.
  // "cancelled" — user/system cancellation.
  reason: "finished" | "failed" | "cancelled";
}

// Extension seam — folded for bot-specific cards (Roadmap, Issues,
// Survey, PlanHandedOff in whats-next). The payload is opaque to the
// generic renderer; bot-specific code post-processes it into typed
// cards downstream.
export interface ExtensionMessage {
  kind: "extension";
  id: string;
  // Tag chosen by the resolver (e.g. "roadmap", "issues-summary"). The
  // bot-specific post-processor switches on it.
  tag: string;
  // Opaque payload — the resolver decides the shape.
  payload: unknown;
}

// Lifecycle of an operator-queued chat message. Stays in lockstep with
// `pkg/store/user_messages.go:QueuedMessageStatus`:
//   queued    → sitting in the inbox, not yet seen by the LLM
//   delivered → injected into the agent's conversation; the next LLM
//               turn will read it (but hasn't been processed yet)
//   consumed  → the LLM has finished a turn that included it
//   cancelled → operator (or runtime) dropped it before delivery
export type UserMessageStatus =
  | "queued"
  | "delivered"
  | "consumed"
  | "cancelled";

// UserMessage is one operator-queued chat message rendered inline in
// the transcript. Position in the message list is anchored to the
// originating `user_message_queued` event's sequence number, so the
// card stays in chronological order alongside the bot's turns.
// Subsequent `user_message_delivered` / `_consumed` / `_cancelled`
// events flip `status` in place without changing the card's position.
export interface UserMessage {
  kind: "user-message";
  id: string; // matches QueuedUserMessage.id
  text: string;
  status: UserMessageStatus;
}

// AssistantTextMessage is one chunk of the agent's mid-turn narration
// (the assistant's prose between tool calls), folded from
// `assistant_text` events. Rendered as the agent's speech bubble so a
// long working step reads as the agent talking, not a frozen banner.
export interface AssistantTextMessage {
  kind: "assistant-text";
  id: string; // "<nodeId>:<iteration>:txt:<seq>"
  nodeId: string;
  iteration: number;
  text: string;
}

export type RunChatMessage =
  | BannerMessage
  | HumanQuestionMessage
  | NodeOutputMessage
  | SessionClosedMessage
  | ExtensionMessage
  | UserMessage
  | AssistantTextMessage;
