import type { RunStatus } from "@/api/runs";

/**
 * Host action requests are published before the assistant reaches its chat
 * pause.  A chat pause can be represented by a checkpoint alone, without a
 * reconstructed human-question message, so the action offer must not depend
 * on that message existing.  An ask_user pause is the one exception: its
 * action list belongs to the previous turn and must stay hidden.
 */
export function shouldRenderAssistantActionOffer(opts: {
  runStatus: RunStatus | null;
  hasPendingHumanQuestion: boolean;
  pendingIsAskUser: boolean;
}): boolean {
  if (opts.runStatus !== "paused_waiting_human") return false;
  return !opts.hasPendingHumanQuestion || !opts.pendingIsAskUser;
}
