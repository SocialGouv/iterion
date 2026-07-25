import { describe, expect, it } from "vitest";

import type { ForgeTeamRepo } from "@/api/forgeConnections";
import type { ScheduledBot } from "@/api/schedules";
import { policyValueFromSchedule } from "@/components/shared/SchedulePolicyEditor";
import {
  UNLINKED_LABEL,
  buildCreatePayload,
  cadenceLabel,
  filterGroupsByRepo,
  formatNextFire,
  formatSeconds,
  groupSchedulesByRepo,
  isAlwaysOn,
  nextUpcoming,
  repoSlugFromUrl,
} from "./scheduleModel";

function repo(over: Partial<ForgeTeamRepo>): ForgeTeamRepo {
  return {
    connection_id: "c1",
    provider: "github",
    repo_full_name: "acme/app",
    integration_id: "int-1",
    bot_ids: ["feature-dev"],
    sync_issues_enabled: false,
    ...over,
  };
}

function sched(over: Partial<ScheduledBot>): ScheduledBot {
  return {
    id: "s1",
    tenant_id: "t1",
    bot_id: "feature-dev",
    cron: "0 2 * * *",
    next_fire_at: "2026-07-19T02:00:00Z",
    created_at: "2026-07-01T00:00:00Z",
    updated_at: "2026-07-01T00:00:00Z",
    ...over,
  };
}

describe("repoSlugFromUrl", () => {
  it("strips protocol, host and .git", () => {
    expect(repoSlugFromUrl("https://github.com/acme/app.git")).toBe("acme/app");
    expect(repoSlugFromUrl("https://gitlab.example.com/grp/sub/app")).toBe("grp/sub/app");
    expect(repoSlugFromUrl("git@github.com:acme/app.git")).toBe("acme/app");
    expect(repoSlugFromUrl("git+ssh://git@host/acme/app/")).toBe("acme/app");
  });
});

describe("groupSchedulesByRepo", () => {
  const repos = [
    repo({ integration_id: "int-1", repo_full_name: "acme/app", clone_url: "https://github.com/acme/app.git" }),
    repo({ integration_id: "int-2", repo_full_name: "acme/zeta", connection_id: "c2" }),
  ];

  it("joins by repo_integration_id first", () => {
    const groups = groupSchedulesByRepo(
      [sched({ id: "a", repo_integration_id: "int-2", repo_url: "https://github.com/acme/app.git" })],
      repos,
    );
    expect(groups).toHaveLength(1);
    expect(groups[0]!.key).toBe("acme/zeta");
    expect(groups[0]!.repo?.integration_id).toBe("int-2");
  });

  it("falls back to the repo_url slug against full name or clone_url", () => {
    const groups = groupSchedulesByRepo(
      [
        sched({ id: "a", repo_url: "https://github.com/acme/app.git" }),
        sched({ id: "b", repo_url: "https://github.com/ACME/APP" }),
      ],
      repos,
    );
    expect(groups).toHaveLength(1);
    expect(groups[0]!.key).toBe("acme/app");
    expect(groups[0]!.schedules.map((s) => s.id).sort()).toEqual(["a", "b"]);
  });

  it("keeps unknown-repo schedules under their slug, unlinked last", () => {
    const groups = groupSchedulesByRepo(
      [
        sched({ id: "a" }),
        sched({ id: "b", repo_url: "https://github.com/other/repo.git" }),
        sched({ id: "c", repo_integration_id: "int-1" }),
      ],
      repos,
    );
    expect(groups.map((g) => g.key)).toEqual(["acme/app", "other/repo", null]);
    expect(groups[2]!.label).toBe(UNLINKED_LABEL);
    expect(groups[1]!.repo).toBeNull();
  });

  it("sorts a group's schedules by bot id", () => {
    const groups = groupSchedulesByRepo(
      [
        sched({ id: "a", bot_id: "willy", repo_integration_id: "int-1" }),
        sched({ id: "b", bot_id: "appy", repo_integration_id: "int-1" }),
      ],
      repos,
    );
    expect(groups[0]!.schedules.map((s) => s.bot_id)).toEqual(["appy", "willy"]);
  });
});

describe("filterGroupsByRepo", () => {
  it("returns everything without a key and narrows case-insensitively with one", () => {
    const groups = groupSchedulesByRepo(
      [sched({ id: "a", repo_url: "https://github.com/acme/app.git" }), sched({ id: "b" })],
      [repo({})],
    );
    expect(filterGroupsByRepo(groups, null)).toHaveLength(2);
    const only = filterGroupsByRepo(groups, "ACME/App");
    expect(only).toHaveLength(1);
    expect(only[0]!.key).toBe("acme/app");
    // The unlinked group never matches a repo filter.
    expect(filterGroupsByRepo(groups, "nope/nope")).toHaveLength(0);
  });
});

describe("formatNextFire", () => {
  const now = Date.parse("2026-07-18T12:00:00Z");
  it("renders future instants as 'in …' and past ones as due", () => {
    expect(formatNextFire("2026-07-18T12:00:30Z", now)).toBe("in 30s");
    expect(formatNextFire("2026-07-18T12:45:00Z", now)).toBe("in 45m");
    expect(formatNextFire("2026-07-18T15:00:00Z", now)).toBe("in 3h");
    expect(formatNextFire("2026-07-21T12:00:00Z", now)).toBe("in 3d");
    expect(formatNextFire("2026-07-18T11:00:00Z", now)).toBe("due now");
    expect(formatNextFire("not-a-date", now)).toBe("not-a-date");
  });
});

describe("nextUpcoming", () => {
  it("picks the earliest enabled schedule, skipping paused ones", () => {
    const a = sched({ id: "a", next_fire_at: "2026-07-19T02:00:00Z" });
    const b = sched({ id: "b", next_fire_at: "2026-07-18T13:00:00Z" });
    const paused = sched({ id: "p", disabled: true, next_fire_at: "2026-07-18T12:30:00Z" });
    expect(nextUpcoming([a, paused, b])?.id).toBe("b");
    expect(nextUpcoming([paused])).toBeNull();
    expect(nextUpcoming([])).toBeNull();
  });
});

describe("keepalive helpers", () => {
  it("isAlwaysOn detects interval or keepalive overlap", () => {
    expect(isAlwaysOn(sched({ interval_seconds: 30 }))).toBe(true);
    expect(isAlwaysOn(sched({ overlap: "keepalive" }))).toBe(true);
    expect(isAlwaysOn(sched({ cron: "0 2 * * *" }))).toBe(false);
  });

  it("formatSeconds compacts to s/m/h", () => {
    expect(formatSeconds(30)).toBe("30s");
    expect(formatSeconds(120)).toBe("2m");
    expect(formatSeconds(7200)).toBe("2h");
  });

  it("cadenceLabel shows always-on cadence or the cron", () => {
    expect(cadenceLabel(sched({ interval_seconds: 30, overlap: "keepalive", cron: "" }))).toBe(
      "always-on · every 30s",
    );
    expect(cadenceLabel(sched({ cron: "0 2 * * *" }))).toBe("0 2 * * *");
  });
});

describe("buildCreatePayload", () => {
  const policy = policyValueFromSchedule();

  it("builds an always-on body from interval, forcing overlap keepalive and omitting cron", () => {
    const body = buildCreatePayload({
      botId: "daemon",
      cron: "",
      repo: null,
      policy,
      alwaysOn: true,
      intervalSeconds: 30,
      staleAfter: "10m",
    });
    expect(body).toEqual({
      bot_id: "daemon",
      interval_seconds: 30,
      overlap: "keepalive",
      stale_after: "10m",
    });
    expect(body.cron).toBeUndefined();
  });

  it("builds a minimal body for an untouched policy and no repo", () => {
    expect(
      buildCreatePayload({ botId: " feature-dev ", cron: " 0 2 * * * ", repo: null, policy }),
    ).toEqual({ bot_id: "feature-dev", cron: "0 2 * * *" });
  });

  it("threads non-empty vars and drops blank keys", () => {
    expect(
      buildCreatePayload({
        botId: "feed-watch",
        cron: "0 8 * * 1",
        repo: null,
        policy,
        vars: { mode: "digest", category: "a11y", "": "ignored", "  ": "x" },
      }).vars,
    ).toEqual({ mode: "digest", category: "a11y" });
  });

  it("omits vars when every key is blank", () => {
    expect(
      buildCreatePayload({ botId: "b", cron: "0 * * * *", repo: null, policy, vars: { "": "x" } })
        .vars,
    ).toBeUndefined();
  });

  it("binds the repo via clone_url, falling back to web_url", () => {
    const withClone = repo({ clone_url: "https://github.com/acme/app.git" });
    expect(
      buildCreatePayload({ botId: "b", cron: "0 * * * *", repo: withClone, policy }).repo_url,
    ).toBe("https://github.com/acme/app.git");
    const webOnly = repo({ web_url: "https://github.com/acme/app" });
    expect(
      buildCreatePayload({ botId: "b", cron: "0 * * * *", repo: webOnly, policy }).repo_url,
    ).toBe("https://github.com/acme/app");
  });

  it("carries the overlap/guard policy, dropping cap and guard extras when inert", () => {
    const full = buildCreatePayload({
      botId: "b",
      cron: "0 * * * *",
      repo: null,
      policy: {
        overlap: "allow",
        maxConcurrent: "3",
        guard: "true",
        guardTimeout: "30s",
        guardVar: "out",
      },
    });
    expect(full.overlap).toBe("allow");
    expect(full.max_concurrent).toBe(3);
    expect(full.guard).toBe("true");
    expect(full.guard_timeout).toBe("30s");
    expect(full.guard_var).toBe("out");

    // max_concurrent is meaningless under skip; guard extras without a guard.
    const inert = buildCreatePayload({
      botId: "b",
      cron: "0 * * * *",
      repo: null,
      policy: {
        overlap: "skip",
        maxConcurrent: "3",
        guard: "  ",
        guardTimeout: "30s",
        guardVar: "out",
      },
    });
    expect(inert.overlap).toBeUndefined();
    expect(inert.max_concurrent).toBeUndefined();
    expect(inert.guard).toBeUndefined();
    expect(inert.guard_timeout).toBeUndefined();
    expect(inert.guard_var).toBeUndefined();
  });
});
