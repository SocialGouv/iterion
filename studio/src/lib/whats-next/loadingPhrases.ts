// Loading phrases for the whats-next chat. Picked to match the actual
// activity (instead of a generic "Working…") so the user gets a
// glanceable hint of where the bot is spending its time. The ticker
// rotates every ~2.4s; on transition out of the loading state the
// indicator unmounts.
//
// Adding a new node? Drop a list under its bot's main.bot node id —
// 4-8 short phrases is the sweet spot (long enough to feel varied,
// short enough that the user reads each one before it rotates out).

import type { RunStatus } from "@/api/runs";

export const NODE_LOADING_PHRASES: Record<string, readonly string[]> = {
  explore: [
    "Counting source files",
    "Peeking into docs",
    "Following git blame trails",
    "Decoding the architecture",
    "Listing the TODOs",
    "Sampling examples",
    "Reading between the lines",
    "Mapping the layout",
  ],
  propose_roadmap: [
    "Drafting the path forward",
    "Connecting the dots",
    "Weighing what matters",
    "Filtering noise from signal",
    "Sorting by impact",
    "Composing the roadmap",
    "Pairing items with bots",
  ],
  carry_roadmap: ["Carrying the latest draft"],
  revise_roadmap: [
    "Folding in your feedback",
    "Adjusting the plan",
    "Re-weighing priorities",
    "Smoothing the edges",
    "Patching the rough spots",
  ],
  emit_action: [
    "Materialising issues",
    "Knocking on the kanban",
    "Filing roadmap items",
    "Assigning to bots",
    "Tagging horizons",
    "Locking in the plan",
    "Stamping the audit trail",
  ],
};

// Used when no per-node list matches — keeps the indicator alive
// instead of falling back to a static ellipsis on a node we haven't
// curated.
export const GENERIC_LOADING_PHRASES: readonly string[] = [
  "Thinking",
  "Cogitating",
  "Consulting the model",
  "Reasoning carefully",
  "Weighing options",
];

export function phrasesForNode(nodeId: string): readonly string[] {
  return NODE_LOADING_PHRASES[nodeId] ?? GENERIC_LOADING_PHRASES;
}

// Preflight phrases — what the PreFlightPanel ticker shows before the
// first whats-next-known node fires. Status-keyed so the words track
// what's actually happening: queued runs say "waiting for a runner",
// freshly-launched runs say "wiring the executor", running-but-silent
// runs say "connecting the stream", etc.

const PREFLIGHT_LAUNCHING: readonly string[] = [
  "Wiring the executor",
  "Loading the bundle",
  "Resolving the backend",
  "Starting MCP servers",
  "Booting the sandbox",
  "Warming up the model",
  "Mirroring skills",
  "Opening the run console",
];

const PREFLIGHT_QUEUED: readonly string[] = [
  "Waiting for a runner",
  "Holding in the queue",
  "Queued behind other runs",
  "Waiting for a slot",
];

const PREFLIGHT_FIRST_EVENT: readonly string[] = [
  "Connecting the stream",
  "Tailing the event log",
  "Catching up on events",
  "Waiting for the first signal",
];

const PREFLIGHT_PREPARING: readonly string[] = [
  "Preparing the first step",
  "Loading attachments",
  "Resolving tools",
  "Compiling the workflow",
  "Allocating budget",
  "Setting up the workspace",
];

const PREFLIGHT_AWAITING_HUMAN: readonly string[] = [
  "Waiting for your input",
  "Looking up the form schema",
  "Paused for a human turn",
];

const PREFLIGHT_FALLBACK: readonly string[] = [
  "Holding steady",
  "Standing by",
  "Waiting on the engine",
];

export function phrasesForPreflight(
  status: "idle" | "launching" | "active" | "submitting" | "ended",
  runStatus: RunStatus | null,
  rawEventCount: number,
): readonly string[] {
  if (status === "launching" || runStatus === null) return PREFLIGHT_LAUNCHING;
  if (runStatus === "queued") return PREFLIGHT_QUEUED;
  if (runStatus === "running" && rawEventCount === 0)
    return PREFLIGHT_FIRST_EVENT;
  if (runStatus === "running") return PREFLIGHT_PREPARING;
  if (runStatus === "paused_waiting_human") return PREFLIGHT_AWAITING_HUMAN;
  return PREFLIGHT_FALLBACK;
}
