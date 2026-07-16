import { describe, expect, it } from "vitest";

import type { PipelineBoardCard } from "@/api/pipelineBoards";

import { findFollowCard } from "./selection";

function card(partial: Partial<PipelineBoardCard>): PipelineBoardCard {
  return {
    id: "card",
    kind: "run",
    column_id: "in_progress",
    title: "Card",
    executed_nodes: 0,
    total_nodes: 0,
    tree_executed_nodes: 0,
    tree_total_nodes: 0,
    created_at: "",
    updated_at: "",
    ...partial,
  };
}

describe("findFollowCard", () => {
  it("matches by id first", () => {
    const anchor = card({ id: "run:1" });
    const cards = [card({ id: "run:1", title: "fresh" })];
    expect(findFollowCard(cards, anchor)?.title).toBe("fresh");
  });

  it("follows a task→run transition by issue id when the id changed", () => {
    const anchor = card({ id: "task:iss-9", kind: "task", issue_id: "iss-9" });
    const cards = [card({ id: "run:abc", kind: "run", issue_id: "iss-9", run_id: "abc" })];
    expect(findFollowCard(cards, anchor)?.id).toBe("run:abc");
  });

  it("falls back to run id when neither id nor issue id match", () => {
    const anchor = card({ id: "run:old", run_id: "abc" });
    const cards = [card({ id: "run:new", run_id: "abc" })];
    expect(findFollowCard(cards, anchor)?.id).toBe("run:new");
  });

  it("returns null when the card has left the board", () => {
    const anchor = card({ id: "run:gone", issue_id: "iss-1", run_id: "r-1" });
    expect(findFollowCard([card({ id: "run:other" })], anchor)).toBeNull();
  });
});
