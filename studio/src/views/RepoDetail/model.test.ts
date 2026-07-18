import { describe, expect, it } from "vitest";

import type { ForgeIntegration, ForgeTeamRepo } from "@/api/forgeConnections";
import type { ScheduledBot } from "@/api/schedules";
import type { TriggerSubscription } from "@/api/triggers";

import {
  integrationForRepo,
  repoSummary,
  schedulesForRepo,
  triggersForRepo,
} from "./model";

const repo: ForgeTeamRepo = {
  connection_id: "c1",
  provider: "github",
  repo_full_name: "owner/repo",
  clone_url: "https://github.com/owner/repo.git",
  web_url: "https://github.com/owner/repo",
  integration_id: "int-1",
  bot_ids: ["revi", "featurly"],
  sync_issues_enabled: false,
};

function integration(over: Partial<ForgeIntegration>): ForgeIntegration {
  return {
    id: "int-1",
    connection_id: "c1",
    provider: "github",
    repo_full_name: "owner/repo",
    bot_ids: ["revi"],
    events_normalized: ["pr.opened"],
    webhook_id: "w1",
    hook_id: "h1",
    created_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

function schedule(over: Partial<ScheduledBot>): ScheduledBot {
  return {
    id: "s1",
    tenant_id: "t1",
    bot_id: "vigie",
    cron: "0 6 * * *",
    next_fire_at: "2026-07-18T06:00:00Z",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

describe("integrationForRepo", () => {
  it("matches by the aggregator's integration_id", () => {
    const hit = integration({});
    expect(integrationForRepo([integration({ id: "other" }), hit], repo)).toBe(hit);
  });

  it("falls back to (connection, repo full name)", () => {
    const hit = integration({ id: "renamed" });
    expect(integrationForRepo([hit], { ...repo, integration_id: "stale" })).toBe(hit);
  });

  it("returns null when nothing matches", () => {
    expect(
      integrationForRepo([integration({ repo_full_name: "other/repo" })], {
        ...repo,
        integration_id: "nope",
      }),
    ).toBeNull();
  });
});

describe("schedulesForRepo", () => {
  it("joins by repo_integration_id", () => {
    const mine = schedule({ repo_integration_id: "int-1" });
    const other = schedule({ id: "s2", repo_integration_id: "int-9" });
    expect(schedulesForRepo([mine, other], repo)).toEqual([mine]);
  });

  it("joins by repo_url slug against the clone_url", () => {
    const mine = schedule({ repo_url: "https://github.com/owner/repo.git" });
    const other = schedule({ id: "s2", repo_url: "https://github.com/owner/other.git" });
    expect(schedulesForRepo([mine, other], repo)).toEqual([mine]);
  });

  it("excludes unlinked schedules", () => {
    expect(schedulesForRepo([schedule({})], repo)).toEqual([]);
  });
});

describe("triggersForRepo", () => {
  const sub = (over: Partial<TriggerSubscription>): TriggerSubscription => ({
    id: "tr1",
    bot_id: "revi",
    invocation: "board",
    match: {},
    enabled: true,
    ...over,
  });

  it("keeps only subscriptions bound to the repo (case-insensitive)", () => {
    const mine = sub({ repo: "Owner/Repo" });
    const unbound = sub({ id: "tr2" });
    const other = sub({ id: "tr3", repo: "owner/other" });
    expect(triggersForRepo([mine, unbound, other], "owner/repo")).toEqual([mine]);
  });
});

describe("repoSummary", () => {
  it("pluralizes both counts", () => {
    expect(repoSummary(1, 1)).toBe("1 bot · 1 schedule");
    expect(repoSummary(2, 0)).toBe("2 bots · 0 schedules");
  });
});
