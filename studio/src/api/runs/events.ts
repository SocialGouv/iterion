// RunEvent — the discriminated union over the run event stream
// (WS `/ws/runs/:id` envelopes + REST `/api/runs/:id/events`).
//
// Mirror of pkg/store/event.go: `Event` supplies the envelope fields,
// `EventType` the discriminant. Payload (`data`) fields are typed
// exactly as far as the studio consumes them; every variant keeps an
// open `[key: string]: unknown` index signature because the Go side
// writes `map[string]any` and emitters may carry fields the UI does
// not (yet) read. The union is CLOSED over the event types the
// backend defines today — raw JSON is stamped as `RunEvent` once, at
// the ingress boundary (`asRunEvent`), and everything downstream
// narrows on `type` instead of casting per-field.
//
// Field provenance (do not invent shapes — derive from the emitters):
//   node lifecycle      pkg/runtime/branch.go, resume.go
//   tool lifecycle      pkg/backend/model/hooks.go
//   human interaction   pkg/runtime/resume.go, review.go
//   run termination     pkg/runtime/run_failure.go
//   browser / preview   pkg/store/event.go doc comments
//   user messages       pkg/store/user_messages.go (QueuedUserMessage)

// ---------------------------------------------------------------------------
// Envelope
// ---------------------------------------------------------------------------

// Fields shared by every event — mirror of store.Event minus Type/Data.
export interface RunEventBase {
  seq: number;
  timestamp: string;
  run_id: string;
  branch_id?: string;
  node_id?: string;
  // Tenant partition key — stamped in cloud mode, absent locally.
  tenant_id?: string;
  // Byte position in the run's log buffer at the moment this event
  // was persisted. Stamped by the backend store from the per-run log
  // buffer total. Used by the time-travel scrubber / replay to slice
  // the live log "up to where the log was when this event fired".
  // Absent on legacy events (pre-feature) and on cloud-mode events
  // where there is no on-host log buffer to attach.
  log_offset?: number;
  // Engine-authoritative active duration (ms since run start, monotonic
  // clock — excludes OS suspend). 0/absent when unknown.
  active_ms?: number;
}

// ---------------------------------------------------------------------------
// Individually-modelled variants (payloads the studio reads by field)
// ---------------------------------------------------------------------------

export interface NodeStartedEvent extends RunEventBase {
  type: "node_started";
  data?: {
    // IR node kind ("agent" | "judge" | "router" | …) as emitted by the
    // runtime; routers also stamp mode/selection fields (index sig).
    kind?: string;
    // Runtime-supplied loop-counter iteration. Absent on legacy events.
    iteration?: number;
    // Stable per-containing-loop counter path ("a=1;b=0"). Absent on
    // legacy events; preferred over `iteration` for exec identity.
    iteration_path?: string;
    [key: string]: unknown;
  };
}

export interface NodeFinishedEvent extends RunEventBase {
  type: "node_finished";
  data?: {
    // The node's structured output, stamped verbatim.
    output?: Record<string, unknown>;
    // Flat cost/token accounting for the step (reducer-derived totals).
    _cost_usd?: number;
    _tokens?: number;
    [key: string]: unknown;
  };
}

// Shared payload for the tool lifecycle trio — emitted by the
// claude_code PreToolUse/PostToolUse hooks and the claw tool loop
// (pkg/backend/model/hooks.go).
export interface ToolEventData {
  tool?: string;
  // Correlates start↔completion on backends that surface one
  // (claude_code); absent on the claw single-tool loop.
  tool_use_id?: string;
  // Tool input — object for structured tools, JSON string on some
  // paths; shape is tool-specific.
  input?: unknown;
  // Some emitters use `arguments` instead of `input`.
  arguments?: unknown;
  error?: string;
  [key: string]: unknown;
}

export interface ToolStartedEvent extends RunEventBase {
  type: "tool_started";
  data?: ToolEventData;
}

export interface ToolCalledEvent extends RunEventBase {
  type: "tool_called";
  data?: ToolEventData;
}

export interface ToolErrorEvent extends RunEventBase {
  type: "tool_error";
  data?: ToolEventData;
}

export interface ArtifactWrittenEvent extends RunEventBase {
  type: "artifact_written";
  data?: {
    publish?: boolean;
    version?: number;
    [key: string]: unknown;
  };
}

// Agent mid-turn narration (data: {text, iteration} per event.go).
export interface AssistantTextEvent extends RunEventBase {
  type: "assistant_text";
  data?: {
    text?: string;
    iteration?: number;
    [key: string]: unknown;
  };
}

// Engine retried after a transient delegate failure.
export interface NodeRecoveryEvent extends RunEventBase {
  type: "node_recovery";
  data?: {
    error?: string;
    [key: string]: unknown;
  };
}

// One turn of a review-gate dialogue as carried on the paused event —
// wire twin of runChat's ReviewTurn.
export interface ReviewTurnWire {
  role: "companion" | "human";
  content?: string;
  verdict?: Record<string, unknown>;
  at?: string;
}

export interface HumanInputRequestedEvent extends RunEventBase {
  type: "human_input_requested";
  data?: {
    interaction_id?: string;
    // Runtime-resolved question field definitions (labels/hints/values);
    // ask_user pauses carry `ask_user_response`, recovery pauses
    // `acknowledge_recovery` inside it.
    questions?: Record<string, unknown>;
    // Resolved `instructions:` prompt of the human node, when declared.
    instructions?: string;
    // Guided review-&-merge gate (interaction: review) — set true with
    // the companion dialogue + merge config alongside.
    review?: boolean;
    turns?: ReviewTurnWire[];
    posture?: string;
    merge_strategy?: string;
    merge_into?: string;
    max_turns?: number;
    review_url?: string;
    verdict?: Record<string, unknown>;
    [key: string]: unknown;
  };
}

export interface HumanAnswersRecordedEvent extends RunEventBase {
  type: "human_answers_recorded";
  data?: {
    interaction_id?: string;
    answers?: Record<string, unknown>;
    [key: string]: unknown;
  };
}

// Async question answered (ADR-081, ask_user_async): fires while the
// run keeps executing — the non-blocking twin of human_answers_recorded.
export interface InteractionAnsweredEvent extends RunEventBase {
  type: "interaction_answered";
  data?: {
    interaction_id?: string;
    async?: boolean;
    answer?: string;
    [key: string]: unknown;
  };
}

// isAsyncHumanInput reports whether a human_input_requested event marks a
// NON-BLOCKING async question (ADR-081, ask_user_async): the run keeps
// executing, so pause-driven consumers (exec-status reducers, pause forms)
// must skip it. TS twin of Go's store.IsAsyncHumanInput.
export function isAsyncHumanInput(evt: {
  type: string;
  data?: Record<string, unknown> | null;
}): boolean {
  return evt.type === "human_input_requested" && evt.data?.["async"] === true;
}

export interface RunPausedEvent extends RunEventBase {
  type: "run_paused";
  data?: {
    // "operator" (POST /pause) | "cost_cap_daily" (daily spend cap) |
    // ""/"human" (human-input pause).
    reason?: string;
    [key: string]: unknown;
  };
}

export interface RunResumedEvent extends RunEventBase {
  type: "run_resumed";
  data?: Record<string, unknown>;
}

export interface RunFinishedEvent extends RunEventBase {
  type: "run_finished";
  data?: Record<string, unknown>;
}

export interface RunFailedEvent extends RunEventBase {
  type: "run_failed";
  data?: {
    error?: string;
    // RuntimeError code (EXECUTION_FAILED, BUDGET_EXCEEDED, …).
    code?: string;
    resumable?: boolean;
    [key: string]: unknown;
  };
}

export interface RunCancelledEvent extends RunEventBase {
  type: "run_cancelled";
  data?: {
    reason?: string;
    [key: string]: unknown;
  };
}

export interface RunRewoundEvent extends RunEventBase {
  type: "run_rewound";
  data?: {
    // Node ids whose state the rewind invalidated (checkpoint outputs,
    // artifacts, child pointers) — the pivot is always included.
    // Emitted by pkg/runview/rewind.go.
    dropped_nodes?: string[];
    from_node?: string;
    to_node?: string;
    [key: string]: unknown;
  };
}

export interface BrowserSessionStartedEvent extends RunEventBase {
  type: "browser_session_started";
  data?: {
    session_id?: string;
    node_id?: string;
    [key: string]: unknown;
  };
}

export interface BrowserSessionEndedEvent extends RunEventBase {
  type: "browser_session_ended";
  data?: {
    session_id?: string;
    [key: string]: unknown;
  };
}

export interface BrowserScreenshotEvent extends RunEventBase {
  type: "browser_screenshot";
  data?: {
    // store.AttachmentRecord.Name of the persisted PNG/JPEG.
    attachment_name?: string;
    // URL the screenshot is of, when known.
    url?: string;
    // "tool-stdout" | "playwright".
    source?: string;
    tool_call_id?: string;
    [key: string]: unknown;
  };
}

export interface PreviewURLAvailableEvent extends RunEventBase {
  type: "preview_url_available";
  data?: {
    url?: string;
    // Optional hint: "dev-server" | "deploy" | "artifact-html".
    kind?: string;
    // "internal" (proxy through /preview) | "external" (default).
    scope?: string;
    // "tool-stdout" | "runtime".
    source?: string;
    [key: string]: unknown;
  };
}

// The user_message_* quartet all carry the QueuedUserMessage record
// (pkg/store/user_messages.go) as their payload.
export interface UserMessageEventData {
  id?: string;
  text?: string;
  // QueuedMessageStatus on the wire; validated at fold time.
  status?: string;
  queued_at?: string;
  delivered_at?: string | null;
  consumed_at?: string | null;
  cancelled_at?: string | null;
  [key: string]: unknown;
}

export interface UserMessageQueuedEvent extends RunEventBase {
  type: "user_message_queued";
  data?: UserMessageEventData;
}

export interface UserMessageDeliveredEvent extends RunEventBase {
  type: "user_message_delivered";
  data?: UserMessageEventData;
}

export interface UserMessageConsumedEvent extends RunEventBase {
  type: "user_message_consumed";
  data?: UserMessageEventData;
}

export interface UserMessageCancelledEvent extends RunEventBase {
  type: "user_message_cancelled";
  data?: UserMessageEventData;
}

// Convenience union for the inbox fold.
export type UserMessageEvent =
  | UserMessageQueuedEvent
  | UserMessageDeliveredEvent
  | UserMessageConsumedEvent
  | UserMessageCancelledEvent;

// Budget events share {dimension, used, limit} (pkg/runtime/budget.go);
// budget_warning has a second emitter shape for loop liveness stalls
// ({loop, reason, crossings} — pkg/runtime/edges.go), hence everything
// optional.
export interface BudgetEventData {
  // "tokens" | "cost_usd" | "iterations" | "duration". Absent on the
  // liveness-stall warning variant.
  dimension?: string;
  used?: number;
  limit?: number;
  // Set on dimensions that carry no used/limit ratio (cost_usd_unpriced),
  // where it is the whole message rather than a supplement to one.
  detail?: string;
  [key: string]: unknown;
}

export interface BudgetWarningEvent extends RunEventBase {
  type: "budget_warning";
  data?: BudgetEventData;
}

export interface BudgetExceededEvent extends RunEventBase {
  type: "budget_exceeded";
  data?: BudgetEventData;
}

// In-process run-health alert (stall / budget / failure) fanned out by
// pkg/alert's browser sink. Unpersisted, Seq=0, broker-only — see the
// interception note in useRunWebSocket.
export interface AlertEvent extends RunEventBase {
  type: "alert";
  data?: {
    // Source condition ("stall" | "budget_warning" | "budget_exceeded"
    // | "run_failed" | …) — drives the toast tone.
    kind?: string;
    // Pre-rendered headline + detail.
    title?: string;
    reason?: string;
    [key: string]: unknown;
  };
}

// ---------------------------------------------------------------------------
// Pass-through variant — every remaining backend event type
// ---------------------------------------------------------------------------

// Event types the studio consumes opaquely (timeline rows, metrics,
// traces read them via bracket access + local validation) or not at
// all. Kept as an explicit literal union — NOT `string` — so that
// narrowing on the modelled variants above excludes this variant, and
// so a comparison against a type the backend doesn't define is a
// compile error. Extend this list in lockstep with pkg/store/event.go.
export type PassthroughEventType =
  | "run_started"
  | "branch_started"
  | "branch_finished"
  | "branch_abandoned"
  | "llm_request"
  | "llm_prompt"
  | "llm_retry"
  | "node_verified_action"
  | "llm_step_finished"
  | "llm_compacted"
  | "plan_written"
  | "run_steered"
  | "run_health"
  | "run_auto_resumed"
  | "run_workspace_reset"
  | "run_workspace_bank_restored"
  | "run_bank_refused"
  | "run_bank_superseded"
  | "run_bank_attempt"
  | "review_turn"
  | "review_verdict"
  | "review_merged"
  | "join_ready"
  | "edge_selected"
  | "run_interrupted"
  | "delegate_started"
  | "delegate_finished"
  | "delegate_error"
  | "delegate_retry"
  | "model_fallback"
  | "model_drift"
  | "session_degraded"
  | "sandbox_skipped"
  | "sandbox_started"
  | "sandbox_claw_routed_via_runner"
  | "sandbox_host_state_mounted"
  | "sandbox_user_remap"
  | "sandbox_uid_mismatch_warning"
  | "sandbox_devbox_provisioned"
  | "network_blocked"
  | "sandbox_build_started"
  | "sandbox_build_finished"
  | "sandbox_build_failed"
  | "worktree_branch_failed";

export interface PassthroughRunEvent extends RunEventBase {
  type: PassthroughEventType;
  data?: Record<string, unknown>;
}

// ---------------------------------------------------------------------------
// The union + ingress
// ---------------------------------------------------------------------------

export type RunEvent =
  | NodeStartedEvent
  | NodeFinishedEvent
  | ToolStartedEvent
  | ToolCalledEvent
  | ToolErrorEvent
  | ArtifactWrittenEvent
  | AssistantTextEvent
  | NodeRecoveryEvent
  | HumanInputRequestedEvent
  | HumanAnswersRecordedEvent
  | InteractionAnsweredEvent
  | RunPausedEvent
  | RunResumedEvent
  | RunFinishedEvent
  | RunFailedEvent
  | RunCancelledEvent
  | RunRewoundEvent
  | BrowserSessionStartedEvent
  | BrowserSessionEndedEvent
  | BrowserScreenshotEvent
  | PreviewURLAvailableEvent
  | UserMessageQueuedEvent
  | UserMessageDeliveredEvent
  | UserMessageConsumedEvent
  | UserMessageCancelledEvent
  | BudgetWarningEvent
  | BudgetExceededEvent
  | AlertEvent
  | PassthroughRunEvent;

export type RunEventType = RunEvent["type"];

// asRunEvent is THE ingress boundary where raw JSON becomes a typed
// RunEvent — the single place the cast is allowed. Validates the
// envelope shape (payloads stay structurally unchecked: every variant
// keeps its fields optional and consumers re-validate at read time,
// so an unknown/future event type degrades to a pass-through row
// instead of being dropped). Throws on a non-event payload so the WS
// layer's per-message error handling surfaces it instead of feeding
// garbage to the reducers.
export function asRunEvent(raw: unknown): RunEvent {
  if (
    !raw ||
    typeof raw !== "object" ||
    typeof (raw as { type?: unknown }).type !== "string" ||
    typeof (raw as { seq?: unknown }).seq !== "number"
  ) {
    throw new Error("not a run event envelope");
  }
  return raw as RunEvent;
}
