// A BOUNDED standing authorisation, so a multi-step repair does not stall on
// a click at every step.
//
// The per-action policy that already exists is global and permanent: setting
// `run.resume` to "allow" authorises every resume, on every run, forever.
// That is the wrong shape for "let the assistant see THIS run through" — the
// operator wants to delegate one job, not hand over a verb.
//
// A grant is therefore scoped on all four axes an operator actually thinks
// in: which assistant, which target run, which actions, and for how long.
// Anything outside falls back to the ordinary policy, which is `ask`.

import { readJSONFlag, writeJSONFlag } from "@/lib/localStorageFlag";

import type { AssistantActionId } from "./assistantActions";

const GRANTS_KEY = "iterion.assistant.missionGrants";
export const MISSION_GRANT_EVENT = "iterion:assistant-mission-grant";

// Actions a mission grant may ever cover. Deliberately a SHORT list of
// recoverable, run-scoped verbs: rewinding and resuming a run changes where
// that run stands and nothing else, and both are undone by rewinding again.
// Anything that writes to the repository, the board or the editor stays out —
// a standing grant must not be able to author code or move someone's tickets
// while nobody is watching.
export const GRANTABLE_ACTIONS: readonly AssistantActionId[] = [
  "run.rewind",
  "run.resume",
  "run.watch",
  "run.unwatch",
];

export interface MissionGrant {
  assistantRunId: string;
  targetRunId: string;
  actions: AssistantActionId[];
  // expiresAt bounds the blast radius in TIME, which is the axis an operator
  // forgets: a grant given for an evening's debugging must not still be live
  // next week. There is no "forever" value on purpose.
  expiresAt: number;
}

function storedGrants(): MissionGrant[] {
  const value = readJSONFlag<unknown>(GRANTS_KEY, []);
  return Array.isArray(value) ? (value as MissionGrant[]) : [];
}

function live(grants: MissionGrant[], now: number): MissionGrant[] {
  return grants.filter(
    (g) =>
      g &&
      typeof g.assistantRunId === "string" &&
      typeof g.targetRunId === "string" &&
      Array.isArray(g.actions) &&
      typeof g.expiresAt === "number" &&
      g.expiresAt > now,
  );
}

/** Every grant still in force. Expired ones are dropped on read, so a stale
 *  entry can never authorise anything even if the write-side missed it. */
export function listMissionGrants(now = Date.now()): MissionGrant[] {
  return live(storedGrants(), now);
}

export function grantMission(
  grant: Omit<MissionGrant, "expiresAt"> & { ttlMs: number },
  now = Date.now(),
): MissionGrant {
  const next: MissionGrant = {
    assistantRunId: grant.assistantRunId,
    targetRunId: grant.targetRunId,
    // Silently narrow rather than reject: a caller asking for more than the
    // grantable set gets the safe subset, never the whole request denied and
    // never the extra verbs.
    actions: grant.actions.filter((a) => GRANTABLE_ACTIONS.includes(a)),
    expiresAt: now + Math.max(0, grant.ttlMs),
  };
  const others = live(storedGrants(), now).filter(
    (g) => !(g.assistantRunId === next.assistantRunId && g.targetRunId === next.targetRunId),
  );
  writeJSONFlag(GRANTS_KEY, [...others, next]);
  notify();
  return next;
}

export function revokeMission(assistantRunId: string, targetRunId?: string): void {
  const remaining = live(storedGrants(), Date.now()).filter((g) =>
    targetRunId === undefined
      ? g.assistantRunId !== assistantRunId
      : !(g.assistantRunId === assistantRunId && g.targetRunId === targetRunId),
  );
  writeJSONFlag(GRANTS_KEY, remaining);
  notify();
}

/**
 * Whether a grant covers this exact (assistant, target, action) triple.
 *
 * targetRunId is compared, not ignored: a grant to see run A through must not
 * silently authorise the same verb on run B, which is precisely the mistake
 * the global per-action policy makes.
 */
export function missionGrantCovers(
  assistantRunId: string | null | undefined,
  targetRunId: string | null | undefined,
  action: AssistantActionId,
  now = Date.now(),
): boolean {
  if (!assistantRunId || !targetRunId) return false;
  if (!GRANTABLE_ACTIONS.includes(action)) return false;
  return listMissionGrants(now).some(
    (g) =>
      g.assistantRunId === assistantRunId &&
      g.targetRunId === targetRunId &&
      g.actions.includes(action),
  );
}

function notify(): void {
  if (typeof window !== "undefined") {
    window.dispatchEvent(new CustomEvent(MISSION_GRANT_EVENT));
  }
}
