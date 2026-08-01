// Two chat-shaped surfaces can be on screen at the same time on
// /runs/:id, and they do opposite things. Naming them in ONE place is
// what keeps that legible — the alternative (both titled
// "Conversation") is how the ambiguity started.
//
//   Assistant — you ask, it answers. A standing session that follows
//               you across routes and knows what page you are on.
//   Steering  — you push. The text is queued into a LIVE agent's inbox
//               and picked up at its next turn. Nothing replies to you.
//
// The lanes keep their bubbles/panels off each other in the bottom-right
// corner. The assistant owns lane 0 (the canonical corner) because it is
// present on every route: its position must never move under the
// operator. Steering, which only exists on a run, sits one lane over.

import type { DockLane } from "@/components/ChatDock/ChatDockShell";

export const ASSISTANT_TITLE = "Assistant";
export const ASSISTANT_HINT =
  "Ask about what you're looking at — the assistant answers you.";
export const ASSISTANT_LANE: DockLane = 0;

export const STEERING_TITLE = "Steering";
export const STEERING_HINT =
  "Messages here are queued into this run's live agent and picked up at its next turn — this is not an assistant, nothing replies.";
export const STEERING_LANE: DockLane = 1;
