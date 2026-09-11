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
// The assistant owns the canonical floating lane. Steering only exists on
// a run and is permanently rendered in that console's right dock.

import type { DockLane } from "@/components/ChatDock/ChatDockShell";

export const ASSISTANT_TITLE = "Assistant";
export const ASSISTANT_HINT =
  "Ask about what you're looking at — the assistant answers you.";
export const ASSISTANT_LANE: DockLane = 0;

export const STEERING_TITLE = "Steering";
export const STEERING_HINT =
  "Messages here are queued into this run's live agent and picked up at its next turn — this is not an assistant, nothing replies.";
