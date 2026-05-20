// Whimsical, per-node loading phrases for the whats-next chat banners.
// Picked to match the actual activity of each node (instead of a
// generic "Working…") so the user gets a glanceable hint of where
// the bot is spending its time. Phrases rotate every ~2.5s while
// the node is `running`; on transition out of running, the banner
// switches to its summary / error state and the phrase stops.
//
// Adding a new node? Drop a list under its bot's main.bot node id —
// 4-8 short phrases is the sweet spot (long enough to feel varied,
// short enough that the user reads each one before it rotates out).

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

// Used when no per-node list matches — keeps the banner alive instead
// of dropping back to a static ellipsis on a node we haven't curated.
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
