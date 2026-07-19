// Trigger helpers for the Integrations "Bots to enable" picker. Derived from
// a bot's manifest invocations (api/bots.ts Invocation), with a legacy
// forge: block still implying a forge-event trigger.

import type { BotEntry } from "@/api/bots";

export type TriggerGroup = "events" | "commands" | "schedule" | "board";

export const GROUP_ORDER: TriggerGroup[] = [
  "events",
  "commands",
  "schedule",
  "board",
];

export const GROUP_LABELS: Record<TriggerGroup, string> = {
  events: "Reacts to events",
  commands: "Run by /command on a PR or issue",
  schedule: "Scheduled (cron)",
  board: "Picks up tickets via the board",
};

/** isRepoCapable reports whether a bot can be offered in the "Bots to enable"
 *  picker: it declares at least one invocation, or a legacy forge: block. */
export function isRepoCapable(b: BotEntry): boolean {
  return (b.invocations?.length ?? 0) > 0 || !!b.forge;
}

/** triggerGroupsFor returns every interaction-mode group a bot belongs to. */
function triggerGroupsFor(b: BotEntry): Set<TriggerGroup> {
  const g = new Set<TriggerGroup>();
  for (const inv of b.invocations ?? []) {
    if (inv.kind === "forge") g.add("events");
    else if (inv.kind === "command") g.add("commands");
    else if (inv.kind === "schedule") g.add("schedule");
    else if (inv.kind === "board") g.add("board");
  }
  if (b.forge && !g.has("events")) g.add("events");
  return g;
}

/** primaryGroup picks the single group a bot is displayed under (it may
 *  belong to several), by GROUP_ORDER priority, so each bot renders once with
 *  a unique checkbox id while its full trigger set shows as chips. */
export function primaryGroup(b: BotEntry): TriggerGroup {
  const g = triggerGroupsFor(b);
  for (const k of GROUP_ORDER) if (g.has(k)) return k;
  return "board";
}

/** hasSchedule reports whether a bot declares a schedule invocation. */
export function hasSchedule(b: BotEntry): boolean {
  return (b.invocations ?? []).some((i) => i.kind === "schedule");
}

/** scheduleCronFor returns the bot's first schedule invocation's suggested
 *  cron (the default shown in the enable dialog's cron picker), or "". */
export function scheduleCronFor(b: BotEntry): string {
  for (const inv of b.invocations ?? []) {
    if (inv.kind === "schedule") return inv.schedule?.suggested_cron ?? "";
  }
  return "";
}

/** triggerChips summarises how a bot is triggered, for display: /commands,
 *  forge events, and a clock-prefixed cron. Falls back to a legacy forge:
 *  block's events. */
export function triggerChips(b: BotEntry): string[] {
  const chips: string[] = [];
  for (const inv of b.invocations ?? []) {
    if (inv.kind === "command" && inv.command) chips.push(`/${inv.command.name}`);
    else if (inv.kind === "forge" && inv.forge) chips.push(inv.forge.event);
    else if (inv.kind === "schedule" && inv.schedule?.suggested_cron)
      chips.push(`⏱ ${inv.schedule.suggested_cron}`);
    else if (inv.kind === "board") chips.push("board");
  }
  if (chips.length === 0 && b.forge?.events) chips.push(...b.forge.events);
  return chips;
}
