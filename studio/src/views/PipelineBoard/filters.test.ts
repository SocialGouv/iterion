import { describe, expect, it } from "vitest";

import type { PipelineBoardCard } from "@/api/pipelineBoards";

import {
  collectFilterOptions,
  emptyPipelineFilters,
  filterPipelineCards,
  pipelineFiltersActive,
} from "./filters";

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

describe("collectFilterOptions", () => {
  it("derives sorted bot + label vocabularies from the cards", () => {
    const { allBots, allLabels } = collectFilterOptions([
      card({ bot_id: "zeta", labels: ["urgent", "audio"] }),
      card({ bot_id: "alpha", labels: ["audio"] }),
      card({}), // no bot, no labels
    ]);
    expect(allBots).toEqual(["alpha", "zeta"]);
    expect(allLabels).toEqual(["audio", "urgent"]);
  });
});

describe("filterPipelineCards", () => {
  const cards = [
    card({ id: "a", title: "Compose jazz episode", bot_id: "shorts", labels: ["audio"] }),
    card({ id: "b", title: "Render video", body: "final cut", bot_id: "shorts", labels: ["video", "urgent"] }),
    card({ id: "c", title: "Weekly digest", bot_id: "digest", run_id: "run-42", issue_id: "iss-9" }),
  ];

  it("returns the input unchanged when no filter is active", () => {
    expect(filterPipelineCards(cards, emptyPipelineFilters())).toBe(cards);
  });

  it("searches title / body / run id / issue id, case-insensitive", () => {
    const f = emptyPipelineFilters();
    expect(filterPipelineCards(cards, { ...f, query: "JAZZ" }).map((c) => c.id)).toEqual(["a"]);
    expect(filterPipelineCards(cards, { ...f, query: "final cut" }).map((c) => c.id)).toEqual(["b"]);
    expect(filterPipelineCards(cards, { ...f, query: "run-42" }).map((c) => c.id)).toEqual(["c"]);
    expect(filterPipelineCards(cards, { ...f, query: "iss-9" }).map((c) => c.id)).toEqual(["c"]);
    expect(filterPipelineCards(cards, { ...f, query: "nothing-matches" })).toEqual([]);
  });

  it("filters by exact bot", () => {
    const f = { ...emptyPipelineFilters(), bot: "shorts" };
    expect(filterPipelineCards(cards, f).map((c) => c.id)).toEqual(["a", "b"]);
  });

  it("labels combine with AND, mirroring /board", () => {
    const f = emptyPipelineFilters();
    expect(
      filterPipelineCards(cards, { ...f, labels: new Set(["video"]) }).map((c) => c.id),
    ).toEqual(["b"]);
    expect(
      filterPipelineCards(cards, { ...f, labels: new Set(["video", "urgent"]) }).map((c) => c.id),
    ).toEqual(["b"]);
    expect(
      filterPipelineCards(cards, { ...f, labels: new Set(["video", "audio"]) }),
    ).toEqual([]);
  });

  it("composes filters (search + bot + labels)", () => {
    const f = {
      query: "render",
      bot: "shorts",
      labels: new Set(["urgent"]),
    };
    expect(filterPipelineCards(cards, f).map((c) => c.id)).toEqual(["b"]);
    expect(
      filterPipelineCards(cards, { ...f, bot: "digest" }),
    ).toEqual([]);
  });
});

describe("pipelineFiltersActive", () => {
  it("detects any active filter", () => {
    expect(pipelineFiltersActive(emptyPipelineFilters())).toBe(false);
    expect(pipelineFiltersActive({ ...emptyPipelineFilters(), query: " x " })).toBe(true);
    expect(pipelineFiltersActive({ ...emptyPipelineFilters(), bot: "b" })).toBe(true);
    expect(
      pipelineFiltersActive({ ...emptyPipelineFilters(), labels: new Set(["l"]) }),
    ).toBe(true);
  });

  it("emptyPipelineFilters returns fresh Sets (no aliasing across resets)", () => {
    const a = emptyPipelineFilters();
    const b = emptyPipelineFilters();
    a.labels.add("x");
    expect(b.labels.size).toBe(0);
  });
});
