// @vitest-environment jsdom

import { beforeEach, describe, expect, it } from "vitest";

import {
  GRANTABLE_ACTIONS,
  grantMission,
  listMissionGrants,
  missionGrantCovers,
  revokeMission,
} from "./missionGrant";

const ASSISTANT = "copi-1";
const TARGET = "run-a";

describe("mission grant", () => {
  beforeEach(() => localStorage.clear());

  it("scopes on the target run, not just the verb", () => {
    // The whole point. The existing per-action policy set to "allow" would
    // authorise run.resume everywhere, forever — the operator delegating one
    // job would be handing over a verb.
    grantMission({ assistantRunId: ASSISTANT, targetRunId: TARGET, actions: ["run.resume"], ttlMs: 60_000 });
    expect(missionGrantCovers(ASSISTANT, TARGET, "run.resume")).toBe(true);
    expect(missionGrantCovers(ASSISTANT, "run-b", "run.resume")).toBe(false);
    expect(missionGrantCovers("copi-2", TARGET, "run.resume")).toBe(false);
  });

  it("expires, and an expired grant authorises nothing", () => {
    // Time is the axis an operator forgets: a grant given for an evening's
    // debugging must not still be live next week. Read-side enforcement, so a
    // stale entry cannot authorise even if the write-side missed it.
    const now = 1_000_000;
    grantMission(
      { assistantRunId: ASSISTANT, targetRunId: TARGET, actions: ["run.resume"], ttlMs: 5_000 },
      now,
    );
    expect(missionGrantCovers(ASSISTANT, TARGET, "run.resume", now + 4_000)).toBe(true);
    expect(missionGrantCovers(ASSISTANT, TARGET, "run.resume", now + 6_000)).toBe(false);
    expect(listMissionGrants(now + 6_000)).toHaveLength(0);
  });

  it("refuses to grant a verb that writes outside the run", () => {
    // A standing grant must not be able to author code or move tickets while
    // nobody is watching. Narrowed silently rather than rejected wholesale:
    // the caller gets the safe subset, never the extra verbs.
    const granted = grantMission({
      assistantRunId: ASSISTANT,
      targetRunId: TARGET,
      actions: ["run.resume", "editor.save", "board.issue.delete"] as never,
      ttlMs: 60_000,
    });
    expect(granted.actions).toEqual(["run.resume"]);
    expect(missionGrantCovers(ASSISTANT, TARGET, "editor.save")).toBe(false);
    expect(GRANTABLE_ACTIONS).not.toContain("editor.save");
  });

  it("is revocable instantly", () => {
    grantMission({ assistantRunId: ASSISTANT, targetRunId: TARGET, actions: ["run.rewind"], ttlMs: 60_000 });
    revokeMission(ASSISTANT, TARGET);
    expect(missionGrantCovers(ASSISTANT, TARGET, "run.rewind")).toBe(false);
  });

  it("replaces rather than stacks a grant for the same mission", () => {
    grantMission({ assistantRunId: ASSISTANT, targetRunId: TARGET, actions: ["run.rewind"], ttlMs: 60_000 });
    grantMission({ assistantRunId: ASSISTANT, targetRunId: TARGET, actions: ["run.resume"], ttlMs: 60_000 });
    expect(listMissionGrants()).toHaveLength(1);
    expect(missionGrantCovers(ASSISTANT, TARGET, "run.rewind")).toBe(false);
    expect(missionGrantCovers(ASSISTANT, TARGET, "run.resume")).toBe(true);
  });
});

describe("mission grant — what it changes at the decision point", () => {
  beforeEach(() => localStorage.clear());

  it("only covers the run it was granted for", () => {
    // The failure this exists to prevent: an operator delegating "see run A
    // through" must not thereby authorise the same verb on every other run,
    // which is exactly what setting the global policy to "allow" would do.
    grantMission({
      assistantRunId: ASSISTANT,
      targetRunId: TARGET,
      actions: ["run.rewind", "run.resume"],
      ttlMs: 60_000,
    });
    expect(missionGrantCovers(ASSISTANT, TARGET, "run.rewind")).toBe(true);
    expect(missionGrantCovers(ASSISTANT, TARGET, "run.resume")).toBe(true);
    expect(missionGrantCovers(ASSISTANT, "another-run", "run.rewind")).toBe(false);
  });

  it("covers nothing when the target is unknown", () => {
    // An action carrying no run_id cannot be matched against a run-scoped
    // grant, so it must fall back to asking rather than inherit coverage.
    grantMission({ assistantRunId: ASSISTANT, targetRunId: TARGET, actions: ["run.resume"], ttlMs: 60_000 });
    expect(missionGrantCovers(ASSISTANT, null, "run.resume")).toBe(false);
    expect(missionGrantCovers(null, TARGET, "run.resume")).toBe(false);
  });
});
