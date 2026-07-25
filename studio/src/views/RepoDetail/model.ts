import type { ForgeIntegration, ForgeTeamRepo } from "@/api/forgeConnections";
import type { ScheduledBot } from "@/api/schedules";
import type { TriggerSubscription } from "@/api/triggers";
import { groupSchedulesByRepo } from "@/views/Triggers/scheduleModel";

// Pure selectors for the repo detail view: join the repo row (the
// team-wide aggregator) against the integration list, the team's
// schedules, and the trigger subscriptions. Kept free of React so the
// aggregation stays unit-testable.

/** The RepoIntegration row backing a connected-repo row — by the
 *  aggregator's integration_id, falling back to (connection, repo). */
export function integrationForRepo(
  integrations: ForgeIntegration[],
  repo: Pick<ForgeTeamRepo, "integration_id" | "connection_id" | "repo_full_name">,
): ForgeIntegration | null {
  return (
    integrations.find((i) => i.id === repo.integration_id) ??
    integrations.find(
      (i) =>
        i.connection_id === repo.connection_id &&
        i.repo_full_name === repo.repo_full_name,
    ) ??
    null
  );
}

/** This repo's schedules, via the Schedules tab's join (integration id
 *  first, then repo_url slug against full name / clone_url). */
export function schedulesForRepo(
  schedules: ScheduledBot[],
  repo: ForgeTeamRepo,
): ScheduledBot[] {
  const groups = groupSchedulesByRepo(schedules, [repo]);
  return groups.find((g) => g.repo !== null)?.schedules ?? [];
}

/** Trigger subscriptions bound to this repo (tenant-wide rows with no
 *  repo binding are excluded — they belong on /triggers, not here). */
export function triggersForRepo(
  subs: TriggerSubscription[],
  repoFullName: string,
): TriggerSubscription[] {
  const want = repoFullName.toLowerCase();
  return subs.filter((s) => (s.repo ?? "").toLowerCase() === want);
}

/** "N bots · M schedules" header summary. */
export function repoSummary(botCount: number, scheduleCount: number): string {
  const count = (n: number, word: string) => `${n} ${word}${n === 1 ? "" : "s"}`;
  return `${count(botCount, "bot")} · ${count(scheduleCount, "schedule")}`;
}
