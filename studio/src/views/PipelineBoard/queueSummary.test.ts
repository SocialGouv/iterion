import { describe, expect, it } from "vitest";

import type { PipelineBoardCard } from "@/api/pipelineBoards";

import { computeQueueSummary, sortLaunchOrder } from "./queueSummary";

function card(partial: Partial<PipelineBoardCard>): PipelineBoardCard {
  return {
    id: "c",
    kind: "task",
    column_id: "opened",
    title: "T",
    executed_nodes: 0,
    total_nodes: 0,
    tree_executed_nodes: 0,
    tree_total_nodes: 0,
    created_at: "2026-07-01T00:00:00Z",
    updated_at: "2026-07-01T00:00:00Z",
    ...partial,
  };
}

describe("computeQueueSummary", () => {
  it("counts ready / waiting / draft and picks next by priority then age", () => {
    const cards = [
      card({
        id: "a",
        issue_id: "i1",
        ready: true,
        priority: 1,
        created_at: "2026-07-01T00:00:00Z",
        title: "low-old",
      }),
      card({
        id: "b",
        issue_id: "i2",
        ready: true,
        priority: 5,
        created_at: "2026-07-02T00:00:00Z",
        title: "high",
      }),
      card({
        id: "c",
        issue_id: "i3",
        open_blocker_count: 2,
        title: "blocked",
      }),
      card({ id: "d", issue_id: "i4", title: "draft" }),
      card({
        id: "e",
        column_id: "in_progress",
        run_id: "r",
        status: "running",
      }),
    ];
    const s = computeQueueSummary(cards, {
      enabled: true,
      max: 3,
      active: 1,
      waiting: 0,
    });
    expect(s.readyCount).toBe(2);
    expect(s.waitingDepsCount).toBe(1);
    expect(s.draftCount).toBe(1);
    expect(s.slotsFree).toBe(2);
    expect(s.nextUp?.title).toBe("high");
  });
});

describe("sortLaunchOrder", () => {
  it("priority desc then created_at asc", () => {
    const got = sortLaunchOrder([
      card({ id: "old-low", priority: 1, created_at: "2026-07-01T00:00:00Z" }),
      card({ id: "new-high", priority: 5, created_at: "2026-07-10T00:00:00Z" }),
      card({ id: "old-mid", priority: 3, created_at: "2026-07-02T00:00:00Z" }),
      card({ id: "new-mid", priority: 3, created_at: "2026-07-05T00:00:00Z" }),
    ]);
    expect(got.map((c) => c.id)).toEqual([
      "new-high",
      "old-mid",
      "new-mid",
      "old-low",
    ]);
  });
});
