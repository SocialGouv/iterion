import { describe, expect, it } from "vitest";

import type { PipelineBoardCard } from "@/api/pipelineBoards";

import {
  cardRouteKey,
  cardRoutePath,
  findCardByRouteKey,
  parseCardRoute,
} from "./cardRoute";

function card(partial: Partial<PipelineBoardCard>): PipelineBoardCard {
  return {
    id: "card",
    kind: "task",
    column_id: "opened",
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

describe("cardRouteKey / path", () => {
  it("prefers issue_id for stable deep links", () => {
    const c = card({ id: "task:x", issue_id: "native:abc", run_id: "r1" });
    expect(cardRouteKey(c)).toEqual({ kind: "issue", id: "native:abc" });
    expect(cardRoutePath(c)).toBe("/pipelines/cards/issue/native%3Aabc");
  });

  it("falls back to run then card id", () => {
    expect(cardRouteKey(card({ id: "run:r9", run_id: "r9" }))).toEqual({
      kind: "run",
      id: "r9",
    });
    expect(cardRouteKey(card({ id: "task:only" }))).toEqual({
      kind: "id",
      id: "task:only",
    });
  });
});

describe("parseCardRoute + findCardByRouteKey", () => {
  const cards = [
    card({ id: "task:a", issue_id: "native:1", title: "A" }),
    card({ id: "run:r2", run_id: "r2", title: "B" }),
  ];

  it("round-trips issue keys", () => {
    const key = parseCardRoute("issue", encodeURIComponent("native:1"));
    expect(key).toEqual({ kind: "issue", id: "native:1" });
    expect(findCardByRouteKey(cards, key!)?.title).toBe("A");
  });

  it("finds runs by run_id", () => {
    const key = parseCardRoute("run", "r2");
    expect(findCardByRouteKey(cards, key!)?.title).toBe("B");
  });

  it("rejects unknown kinds", () => {
    expect(parseCardRoute("nope", "x")).toBeNull();
  });
});
