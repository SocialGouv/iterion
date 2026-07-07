import { describe, expect, it } from "vitest";

import {
  deriveWatchedIds,
  formatUpdatesAsChatMessage,
  type WatchUpdate,
} from "./useWatchList";

describe("deriveWatchedIds", () => {
  it("uses the server list (Run.WatchedIssueIDs) as the sole source", () => {
    expect(deriveWatchedIds(["native:x", "native:y"], [])).toEqual([
      "native:x",
      "native:y",
    ]);
  });

  it("dedupes while preserving first-seen order", () => {
    expect(
      deriveWatchedIds(["native:x", "native:a", "native:x"], []),
    ).toEqual(["native:x", "native:a"]);
  });

  it("drops empty ids", () => {
    expect(deriveWatchedIds(["native:x", ""], [])).toEqual(["native:x"]);
  });

  it("returns empty when the server list is absent or empty", () => {
    expect(deriveWatchedIds(undefined, [])).toEqual([]);
    expect(deriveWatchedIds([], [])).toEqual([]);
  });
});

describe("formatUpdatesAsChatMessage", () => {
  it("returns empty for no updates", () => {
    expect(formatUpdatesAsChatMessage([])).toBe("");
  });

  it("formats a single transition", () => {
    const upd: WatchUpdate = {
      issueId: "native:abc",
      title: "Fix sandbox doctor",
      prevState: "ready",
      newState: "in_progress",
      at: "2026-05-27T17:00:00Z",
    };
    expect(formatUpdatesAsChatMessage([upd])).toBe(
      [
        "Board updates since last check:",
        "- Fix sandbox doctor: ready → in_progress",
      ].join("\n"),
    );
  });

  it("collapses multi-hop chains per issue to first prev + latest next", () => {
    const issueId = "native:abc";
    const updates: WatchUpdate[] = [
      {
        issueId,
        title: "Doc-align fix",
        prevState: "backlog",
        newState: "ready",
        at: "2026-05-27T17:00:00Z",
      },
      {
        issueId,
        title: "Doc-align fix",
        prevState: "ready",
        newState: "in_progress",
        at: "2026-05-27T17:05:00Z",
      },
      {
        issueId,
        title: "Doc-align fix",
        prevState: "in_progress",
        newState: "review",
        at: "2026-05-27T17:30:00Z",
      },
    ];
    expect(formatUpdatesAsChatMessage(updates)).toBe(
      [
        "Board updates since last check:",
        "- Doc-align fix: backlog → review",
      ].join("\n"),
    );
  });

  it("keeps one entry per issue when multiple issues transition", () => {
    const updates: WatchUpdate[] = [
      {
        issueId: "native:a",
        title: "Issue A",
        prevState: "ready",
        newState: "in_progress",
        at: "2026-05-27T17:00:00Z",
      },
      {
        issueId: "native:b",
        title: "Issue B",
        prevState: "in_progress",
        newState: "done",
        at: "2026-05-27T17:05:00Z",
      },
      {
        issueId: "native:a",
        title: "Issue A",
        prevState: "in_progress",
        newState: "review",
        at: "2026-05-27T17:30:00Z",
      },
    ];
    expect(formatUpdatesAsChatMessage(updates)).toBe(
      [
        "Board updates since last check:",
        "- Issue A: ready → review",
        "- Issue B: in_progress → done",
      ].join("\n"),
    );
  });
});
