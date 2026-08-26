import type { RunEvent } from "@/api/runs";

// Per-event-type accent styles for the row badge. Keys mirror the
// runtime event taxonomy emitted into events.jsonl.
export const EVENT_BADGE: Record<string, string> = {
  run_started: "bg-info-soft text-info-fg",
  run_finished: "bg-success-soft text-success-fg",
  run_failed: "bg-danger-soft text-danger-fg",
  run_paused: "bg-warning-soft text-warning-fg",
  run_resumed: "bg-info-soft text-info-fg",
  run_steered: "bg-info-soft text-info-fg",
  run_health: "bg-warning-soft text-warning-fg",
  run_cancelled: "bg-surface-2 text-fg-muted",
  node_started: "bg-info-soft text-info-fg",
  node_finished: "bg-success-soft text-success-fg",
  artifact_written: "bg-accent-soft text-fg-default",
  human_input_requested: "bg-warning-soft text-warning-fg",
  budget_warning: "bg-warning-soft text-warning-fg",
  // The operator's own subscription ceiling, hit below the provider's
  // wall. Warning-coloured: the run either stops or is the last one to
  // start, and either way it is a decision iterion took.
  usage_cap: "bg-warning-soft text-warning-fg",
  budget_exceeded: "bg-danger-soft text-danger-fg",
  // A route change, not a failure — the run continues on another
  // model/backend. Warning-coloured because the primary route is gone.
  model_fallback: "bg-warning-soft text-warning-fg",
  // Declared model ≠ what the backend actually ran (proxy, env
  // override, fallback route). Same colour as fallback: the record
  // is honest, the run is degraded.
  model_drift: "bg-warning-soft text-warning-fg",
  llm_request: "bg-surface-2 text-fg-muted",
  llm_step_finished: "bg-surface-2 text-fg-muted",
  tool_called: "bg-surface-2 text-fg-muted",
  tool_error: "bg-danger-soft text-danger-fg",
};

export interface AnnotatedEvent {
  event: RunEvent;
  // Scalar `iteration` from the event's data (for display in the row
  // header). NOT unique post-Option-3 — the runtime's
  // currentLoopIteration can return the same max() across multiple
  // attempts when an outer loop counter dominates the inner.
  iteration: number;
  // 0-based count of node_started events for this (branch, node) up to
  // and including this row. Used as the array index in the per-node
  // executions list when the user clicks the row to jump to its exec.
  executionIndex: number;
  // execution_id of the exec this event belongs to (the most recent
  // node_started for this branch+node, or this event's own id if it
  // IS a node_started). Used by the selection filter when cross-
  // highlighting from the canvas.
  executionId: string | null;
  // Pre-computed match against the current filters; cached so the
  // virtual list doesn't recompute per scroll frame.
  preview: string;
}

// indentForType returns a visual nesting level for the event log. The
// goal is to make multi-turn LLM rounds and their tool calls visually
// "owned" by the surrounding llm_request, so the eye can scan
// turn-boundaries instead of treating every event as a sibling. The
// runtime doesn't carry an explicit parent_seq on retries / tool calls,
// so we lean on the event taxonomy itself.
export function indentForType(t: string): number {
  switch (t) {
    case "llm_step_finished":
    case "llm_retry":
    case "tool_started":
    case "tool_called":
    case "tool_error":
    case "human_input_requested":
      return 1;
    case "artifact_written":
      return 1;
    default:
      return 0;
  }
}

export function previewData(data: Record<string, unknown> | undefined): string {
  if (!data) return "";
  const interesting = [
    "kind",
    "model",
    "declared_model",
    "effective_model",
    "tool",
    "tool_name",
    "version",
    "publish",
    "to",
    // model_fallback: the route change and why it happened. to_action
    // ("skip") marks a terminal degrade — to_backend is then empty on
    // purpose, nothing serves the node.
    "reason",
    // A zero-attempt route change is a proactive cooldown skip, not a
    // second provider refusal. Keep that distinction visible in the row
    // preview without requiring operators to open the raw event payload.
    "cooldown",
    "cooldown_until",
    "attempts",
    "from_backend",
    "to_action",
    "to_backend",
    "to_model",
    // delegate_finished on a skipped node: nothing served it.
    "skipped",
    "loop",
    "iteration",
    "error",
    "input_tokens",
    "output_tokens",
  ];
  const parts: string[] = [];
  for (const k of interesting) {
    if (data[k] !== undefined) parts.push(`${k}=${formatValue(data[k])}`);
  }
  return parts.join(" ");
}

export function formatValue(v: unknown): string {
  if (typeof v === "string") {
    return v.length > 60 ? v.slice(0, 57) + "…" : v;
  }
  return String(v);
}
