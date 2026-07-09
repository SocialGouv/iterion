import type { RunStatus } from "@/api/runs";

// composerPlaceholder picks the prompt copy for the always-on
// composer based on the state it's rendered over: answering Nexie's
// pending question, folding into a live step, or re-seeding a fresh
// session after a close.
export function composerPlaceholder(
  runStatus: RunStatus | null,
  hasPendingTurn = false,
): string {
  if (hasPendingTurn) {
    return "Reply to Nexie…";
  }
  if (
    runStatus === "finished" ||
    runStatus === "failed" ||
    runStatus === "cancelled"
  ) {
    return "Send a message to start a fresh Nexie session…";
  }
  return "Message Nexie — it'll fold this into the step it's running…";
}
