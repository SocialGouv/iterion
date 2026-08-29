// What the operator has decided about how a NEW assistant conversation starts.
//
// Two settings, and they are deliberately separate:
//
//   reviewer      — should a second model, from another family, criticise each
//                   answer before it is read. It is real money: a full extra
//                   call per turn (measured at $2.46–2.57 per reviewed turn on
//                   Copi), which is why it is off by default and why the choice
//                   is offered at all rather than buried.
//   askBeforeStart— should the choice be offered each time. Turning it off is
//                   not "forget my preference": it means "use the one I saved,
//                   stop asking". So the value above must keep working when
//                   this is false — the prompt goes away, the setting does not.
//
// Both live in localStorage next to the dock's other per-browser state. They
// are reachable again from Settings → Assistant; a preference you can only set
// once, in a dialog you dismissed, is a trap.

import {
  readBooleanFlag,
  writeBooleanFlag,
} from "@/lib/localStorageFlag";

export const ASSISTANT_REVIEWER_KEY = "iterion.assistant.reviewer";
export const ASSISTANT_ASK_BEFORE_START_KEY = "iterion.assistant.askBeforeStart";

/** Cross-review is OFF by default: it doubles the per-turn spend. */
export function readReviewer(): boolean {
  return readBooleanFlag(ASSISTANT_REVIEWER_KEY, false);
}

export function writeReviewer(on: boolean): void {
  writeBooleanFlag(ASSISTANT_REVIEWER_KEY, on);
}

/** Asking is ON by default: the first conversation should surface the choice. */
export function readAskBeforeStart(): boolean {
  return readBooleanFlag(ASSISTANT_ASK_BEFORE_START_KEY, true);
}

export function writeAskBeforeStart(ask: boolean): void {
  writeBooleanFlag(ASSISTANT_ASK_BEFORE_START_KEY, ask);
}

// The var name a conversational bot reads. A bot opts in by declaring it in
// its manifest's `chat.launcher_vars` — see botDeclaresReviewer below, which
// is what keeps this from being pushed at bots that have no such var.
export const REVIEWER_VAR = "reviewer";

export function reviewerVars(on: boolean): Record<string, string> {
  return { [REVIEWER_VAR]: on ? "on" : "off" };
}

/**
 * botDeclaresReviewer reports whether this bot's manifest says it understands
 * cross-review. The choice is only offered — and only sent — for bots that do.
 */
export function botDeclaresReviewer(bot: {
  launcherVars?: ReadonlyArray<{ name: string }>;
}): boolean {
  return (bot.launcherVars ?? []).some((v) => v.name === REVIEWER_VAR);
}
