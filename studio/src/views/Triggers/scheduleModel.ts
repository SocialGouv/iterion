import type { ForgeTeamRepo } from "@/api/forgeConnections";
import type { ScheduleCreateInput, ScheduledBot } from "@/api/schedules";
import {
  policyFieldsFromValue,
  type SchedulePolicyValue,
} from "@/components/shared/SchedulePolicyEditor";
import { parseGoDuration } from "@/lib/duration";

/** Minimum always-on interval (seconds) — mirrors the server's
 *  bundle.KeepaliveMinInterval floor so the dialog rejects the same values. */
export const KEEPALIVE_MIN_INTERVAL_SEC = 5;

/** Parses an always-on interval string (Go duration, e.g. "30s", "2m") to
 *  whole seconds, or returns an error when it is unparseable or below the
 *  floor. Shared by the create + edit dialogs so the rule lives in one place. */
export function intervalSecondsOrError(v: string): { seconds: number } | { error: string } {
  const ms = parseGoDuration(v);
  const seconds = ms == null ? 0 : Math.floor(ms / 1000);
  if (seconds < KEEPALIVE_MIN_INTERVAL_SEC) {
    return { error: `Interval must be at least ${KEEPALIVE_MIN_INTERVAL_SEC}s (e.g. 30s, 2m).` };
  }
  return { seconds };
}

// Pure model for the Schedules tab: the client-side join of a team's
// schedules against its connected forge repos, plus the create-payload
// assembly. Kept free of React so it stays unit-testable.

/** Normalizes a clone/web URL to an "owner/repo" slug for display and
 *  for matching a schedule's repo_url against a connected repo. */
export function repoSlugFromUrl(url: string): string {
  return url
    .trim()
    .replace(/^git@[^:]+:/, "")
    .replace(/^[a-z+]+:\/\/[^/]+\//i, "")
    .replace(/\.git$/, "")
    .replace(/\/+$/, "");
}

export interface ScheduleRepoGroup {
  /** Filter key: the repo slug ("owner/repo"), or null for unlinked. */
  key: string | null;
  /** Human heading for the group. */
  label: string;
  /** The connected repo row when the join matched one. */
  repo: ForgeTeamRepo | null;
  schedules: ScheduledBot[];
}

export const UNLINKED_LABEL = "Not linked to a repository";

/**
 * Groups schedules by repository. Join order per schedule:
 * repo_integration_id → connected repo's integration_id; else repo_url
 * slug → connected repo's repo_full_name (or clone_url slug); else the
 * raw repo_url slug as its own group; else the "not linked" group.
 * Connected-repo groups sort first (alphabetically), then unmatched-URL
 * groups, with the unlinked group last.
 */
export function groupSchedulesByRepo(
  schedules: ScheduledBot[],
  repos: ForgeTeamRepo[],
): ScheduleRepoGroup[] {
  const groups = new Map<string, ScheduleRepoGroup>();
  const idFor = (g: { key: string | null }) => g.key ?? "\u0000unlinked";

  const groupFor = (s: ScheduledBot): ScheduleRepoGroup => {
    const byIntegration = s.repo_integration_id
      ? repos.find((r) => r.integration_id === s.repo_integration_id)
      : undefined;
    if (byIntegration) {
      return {
        key: byIntegration.repo_full_name,
        label: byIntegration.repo_full_name,
        repo: byIntegration,
        schedules: [],
      };
    }
    if (s.repo_url) {
      const slug = repoSlugFromUrl(s.repo_url);
      const byUrl = repos.find(
        (r) =>
          r.repo_full_name.toLowerCase() === slug.toLowerCase() ||
          (r.clone_url && repoSlugFromUrl(r.clone_url).toLowerCase() === slug.toLowerCase()),
      );
      if (byUrl) {
        return {
          key: byUrl.repo_full_name,
          label: byUrl.repo_full_name,
          repo: byUrl,
          schedules: [],
        };
      }
      return { key: slug, label: slug, repo: null, schedules: [] };
    }
    return { key: null, label: UNLINKED_LABEL, repo: null, schedules: [] };
  };

  for (const s of schedules) {
    const g = groupFor(s);
    const id = idFor(g);
    const existing = groups.get(id);
    if (existing) {
      existing.schedules.push(s);
    } else {
      g.schedules.push(s);
      groups.set(id, g);
    }
  }

  const rank = (g: ScheduleRepoGroup) => (g.repo ? 0 : g.key !== null ? 1 : 2);
  const out = [...groups.values()].sort(
    (a, b) => rank(a) - rank(b) || a.label.localeCompare(b.label),
  );
  for (const g of out) {
    g.schedules.sort(
      (a, b) =>
        a.bot_id.localeCompare(b.bot_id) ||
        cadenceLabel(a).localeCompare(cadenceLabel(b)),
    );
  }
  return out;
}

/** Whether a schedule is an always-on (keepalive) one. */
export function isAlwaysOn(s: Pick<ScheduledBot, "interval_seconds" | "overlap">): boolean {
  return (s.interval_seconds ?? 0) > 0 || s.overlap === "keepalive";
}

/** Human cadence: "always-on · every 30s" for keepalive, else the cron. */
export function cadenceLabel(s: Pick<ScheduledBot, "cron" | "interval_seconds" | "overlap">): string {
  if (isAlwaysOn(s)) {
    const sec = s.interval_seconds ?? 0;
    return sec > 0 ? `always-on · every ${formatSeconds(sec)}` : "always-on";
  }
  return s.cron;
}

/** Compact seconds → "30s" / "5m" / "2h". */
export function formatSeconds(sec: number): string {
  if (sec % 3600 === 0) return `${sec / 3600}h`;
  if (sec % 60 === 0) return `${sec / 60}m`;
  return `${sec}s`;
}

/** Narrows the grouped list to one repo key ("" / null = no filter). */
export function filterGroupsByRepo(
  groups: ScheduleRepoGroup[],
  repoKey: string | null,
): ScheduleRepoGroup[] {
  if (!repoKey) return groups;
  return groups.filter(
    (g) => g.key !== null && g.key.toLowerCase() === repoKey.toLowerCase(),
  );
}

/**
 * Renders a next_fire_at instant relative to now: "in 2h", "in 3d",
 * "due now" once passed (the ticker fires it on its next sweep).
 * lib/format's formatRelative is past-only ("2h ago"), hence this twin.
 */
export function formatNextFire(iso: string, now: number = Date.now()): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return iso;
  const seconds = Math.round((t - now) / 1000);
  if (seconds <= 0) return "due now";
  if (seconds < 60) return `in ${seconds}s`;
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `in ${minutes}m`;
  const hours = Math.round(minutes / 60);
  if (hours < 24) return `in ${hours}h`;
  return `in ${Math.round(hours / 24)}d`;
}

/** The active schedule that fires soonest, or null (all paused / none). */
export function nextUpcoming(schedules: ScheduledBot[]): ScheduledBot | null {
  let best: ScheduledBot | null = null;
  for (const s of schedules) {
    if (s.disabled) continue;
    const t = Date.parse(s.next_fire_at);
    if (Number.isNaN(t)) continue;
    if (!best || t < Date.parse(best.next_fire_at)) best = s;
  }
  return best;
}

export interface ScheduleDraft {
  botId: string;
  cron: string;
  /** The connected repo to bind to, or null for "no repository". */
  repo: ForgeTeamRepo | null;
  policy: SchedulePolicyValue;
  /** Per-run vars passed to the bot on each fire (e.g. a feed-watch digest's
   *  `mode=digest` + `category=a11y`). Empty when the bot needs none. */
  vars?: Record<string, string>;
  /** Always-on (keepalive) mode: relaunch every intervalSeconds with
   *  at-most-one-live + staleness reaping, instead of firing on cron. */
  alwaysOn?: boolean;
  intervalSeconds?: number;
  /** Silence cutoff for always-on (Go duration, e.g. "5m"); empty = default. */
  staleAfter?: string;
}

/**
 * Builds the POST body from the dialog draft. The repo binding travels as
 * repo_url (the create API has no repo_integration_id field) — clone_url
 * preferred, matching what the forge orchestrator stores, so the group
 * join round-trips. Empty policy fields are omitted so an untouched
 * policy creates the same minimal row as the orchestrator's.
 */
export function buildCreatePayload(d: ScheduleDraft): ScheduleCreateInput {
  const input: ScheduleCreateInput = { bot_id: d.botId.trim() };
  const p = policyFieldsFromValue(d.policy);
  if (d.alwaysOn) {
    // Keepalive: interval instead of cron; overlap forced to keepalive.
    input.interval_seconds = Math.max(0, Math.floor(d.intervalSeconds ?? 0));
    input.overlap = "keepalive";
    if (d.staleAfter?.trim()) input.stale_after = d.staleAfter.trim();
  } else {
    input.cron = d.cron.trim();
    if (p.overlap === "allow") {
      input.overlap = "allow";
      if (p.max_concurrent > 0) input.max_concurrent = p.max_concurrent;
    }
  }
  if (d.repo) {
    input.repo_url = d.repo.clone_url || d.repo.web_url || d.repo.repo_full_name;
  }
  if (d.vars) {
    const vars: Record<string, string> = {};
    for (const [k, v] of Object.entries(d.vars)) {
      if (k.trim()) vars[k.trim()] = v;
    }
    if (Object.keys(vars).length > 0) input.vars = vars;
  }
  // The guard applies to both cadences.
  if (p.guard) {
    input.guard = p.guard;
    if (p.guard_timeout) input.guard_timeout = p.guard_timeout;
    if (p.guard_var) input.guard_var = p.guard_var;
  }
  return input;
}
