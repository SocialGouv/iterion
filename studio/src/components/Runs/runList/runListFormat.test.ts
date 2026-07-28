import { describe, expect, it } from "vitest";

import type { RunSummary } from "@/api/runs";

import { retryAttemptLabel, retryHint } from "./runListFormat";

// A run parked on an exhausted provider quota window stays
// failed_resumable while it waits. Without this hint it is visually
// identical to a run that will never come back — which is how an operator
// ends up resuming by hand something already scheduled, or writing off
// something that was going to recover on its own.

const NOW = Date.parse("2026-07-27T06:00:00Z");

function run(over: Partial<RunSummary> = {}): RunSummary {
  return {
    id: "019fa228",
    workflow_name: "feed_watch",
    status: "failed_resumable",
    created_at: "2026-07-27T06:00:00Z",
    updated_at: "2026-07-27T06:00:00Z",
    active: false,
    ...over,
  } as RunSummary;
}

describe("retryHint", () => {
  it("is empty when no retry is armed", () => {
    expect(retryHint(run(), NOW)).toBe("");
  });

  it("renders hours for a same-day-ish reset", () => {
    // The incident's shape: a weekly cap resetting ~39h out.
    expect(
      retryHint(run({ retry_after: "2026-07-28T21:00:00Z" }), NOW),
    ).toBe("retrying in 39h");
  });

  it("renders minutes under an hour", () => {
    expect(
      retryHint(run({ retry_after: "2026-07-27T06:45:00Z" }), NOW),
    ).toBe("retrying in 45m");
  });

  it("renders days past 48h so a weekly wait stays readable", () => {
    expect(
      retryHint(run({ retry_after: "2026-08-02T06:00:00Z" }), NOW),
    ).toBe("retrying in 6d");
  });

  it("never renders a negative duration", () => {
    // The sweeper polls, so "due" and "running again" are a minute apart.
    expect(
      retryHint(run({ retry_after: "2026-07-27T05:00:00Z" }), NOW),
    ).toBe("retrying soon");
  });

  it("rounds a sub-minute wait up rather than showing 0m", () => {
    expect(
      retryHint(run({ retry_after: "2026-07-27T06:00:20Z" }), NOW),
    ).toBe("retrying in 1m");
  });

  it("ignores an unparseable instant instead of rendering NaN", () => {
    expect(retryHint(run({ retry_after: "not-a-date" }), NOW)).toBe("");
  });
});

describe("retryAttemptLabel", () => {
  it("is empty when nothing has been attempted", () => {
    expect(retryAttemptLabel(run())).toBe("");
    expect(retryAttemptLabel(run({ retry_attempts: 0 }))).toBe("");
  });

  it("names the attempt for the tooltip", () => {
    expect(retryAttemptLabel(run({ retry_attempts: 2 }))).toBe(
      "automatic retry 2",
    );
  });
});
