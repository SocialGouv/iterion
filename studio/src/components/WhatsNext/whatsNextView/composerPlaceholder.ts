import type { RunStatus } from "@/api/runs";

// composerPlaceholder picks the prompt copy for the always-on composer based
// on the state it's rendered over: answering the assistant's pending
// question, folding into a live step, or re-seeding a fresh session after a
// close.
//
// `botLabel` is the persona the operator actually picked. It used to be the
// literal "Nexie" — harmless while the chat surface could only ever host one
// bot, and wrong the moment the registry became manifest-driven: switching to
// Copi left the composer still offering to message Nexie, which reads as "the
// switch did nothing".
export function composerPlaceholder(
  runStatus: RunStatus | null,
  hasPendingTurn = false,
  botLabel = "the assistant",
): string {
  const who = botLabel.trim() || "the assistant";
  if (hasPendingTurn) {
    return `Reply to ${who}…`;
  }
  if (
    runStatus === "finished" ||
    runStatus === "failed" ||
    runStatus === "cancelled"
  ) {
    return `Send a message to start a fresh ${who} session…`;
  }
  return `Message ${who} — it'll fold this into the step it's running…`;
}
