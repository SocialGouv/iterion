import { describe, expect, it } from "vitest";

import {
  defaultConventionalMessage,
  mergeBlockedReason,
} from "./CommitsPanel";
import type { RunHeader } from "@/api/runs";

function header(partial: Partial<RunHeader>): RunHeader {
  return {
    id: "run-1",
    workflow_name: "whole_improve_loop",
    status: "finished",
    created_at: "2026-05-28T00:00:00Z",
    updated_at: "2026-05-28T00:00:00Z",
    active_duration_ms: 0,
    ...partial,
  } as RunHeader;
}

describe("defaultConventionalMessage", () => {
  it("strips the [#N] issue prefix and guesses fix from keywords", () => {
    const run = header({
      source: {
        kind: "dispatcher",
        issue_title: "[#2] Doc-align context exhaustion fix — chunk reviewer batches",
      },
    });
    expect(defaultConventionalMessage(run)).toBe(
      "fix: Doc-align context exhaustion fix — chunk reviewer batches",
    );
  });

  it("guesses feat for additive work", () => {
    const run = header({
      source: { kind: "dispatcher", issue_title: "Add inline Dispatch button to cards" },
    });
    expect(defaultConventionalMessage(run)).toBe(
      "feat: Add inline Dispatch button to cards",
    );
  });

  it("passes an already-conventional subject through untouched", () => {
    const run = header({
      source: {
        kind: "dispatcher",
        issue_title: "feat(studio): inline Dispatch button on cards",
      },
    });
    expect(defaultConventionalMessage(run)).toBe(
      "feat(studio): inline Dispatch button on cards",
    );
  });

  it("falls back to the run name when no source issue", () => {
    const run = header({ name: "electric-flash-orbitcrest-0c34", source: undefined });
    expect(defaultConventionalMessage(run)).toBe(
      "chore: electric-flash-orbitcrest-0c34",
    );
  });

  it("guesses docs for documentation work", () => {
    const run = header({
      source: { kind: "dispatcher", issue_title: "Update README and onboarding docs" },
    });
    expect(defaultConventionalMessage(run)).toBe(
      "docs: Update README and onboarding docs",
    );
  });
});

describe("mergeBlockedReason", () => {
  it("blocks when the commit list is unavailable, even with a nonzero count", () => {
    expect(mergeBlockedReason(3, true)).toMatch(/commit list couldn't be read/);
  });

  it("blocks when there are zero commits", () => {
    expect(mergeBlockedReason(0, false)).toMatch(/nothing to merge/i);
  });

  it("allows merging with commits and a healthy list", () => {
    expect(mergeBlockedReason(2, false)).toBeNull();
  });
});
