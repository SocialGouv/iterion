import { describe, expect, it } from "vitest";

import type { PipelineBoardCard } from "@/api/pipelineBoards";

import {
  collectFilterOptions,
  emptyPipelineFilters,
  filterInventoryCards,
  filterPipelineCards,
  partitionPipelineCards,
  pipelineFiltersActive,
  sortInventoryCards,
  sortNewestFirst,
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
      card({}), // no bot, no workflow, no labels
    ]);
    expect(allBots).toEqual(["alpha", "zeta"]);
    expect(allLabels).toEqual(["audio", "urgent"]);
  });

  it("falls back to workflow_name when bot_id is empty (loose .bot runs)", () => {
    // A run launched from a loose main.bot (no manifest.yaml → not a
    // bundle) has no bot identity; its workflow name must still appear in
    // the dropdown or the pipeline is unfilterable.
    const { allBots } = collectFilterOptions([
      card({ workflow_name: "shorts_historical_series" }),
      card({ bot_id: "pipeline-board-demo", workflow_name: "pipeline_board_demo" }),
    ]);
    expect(allBots).toEqual(["pipeline-board-demo", "shorts_historical_series"]);
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

  it("filters by pipeline_kind, family_id, and waiting deps", () => {
    const kindCards = [
      card({
        id: "m",
        entry_input: { pipeline_kind: "mesh", family_id: "fort", input_path: "a.json" },
      }),
      card({
        id: "f",
        entry_input: { pipeline_kind: "feature", family_id: "fort" },
        open_blocker_count: 2,
        issue_state: "waiting_deps",
      }),
      card({ id: "x", entry_input: { pipeline_kind: "mesh", family_id: "other" } }),
    ];
    const base = emptyPipelineFilters();
    expect(
      filterPipelineCards(kindCards, { ...base, pipelineKind: "mesh" }).map((c) => c.id),
    ).toEqual(["m", "x"]);
    expect(
      filterPipelineCards(kindCards, { ...base, familyId: "fort" }).map((c) => c.id),
    ).toEqual(["m", "f"]);
    expect(
      filterPipelineCards(kindCards, { ...base, waitingDepsOnly: true }).map((c) => c.id),
    ).toEqual(["f"]);
  });

  it("bot filter matches the workflow_name fallback for bot-less runs", () => {
    const looseCards = [
      card({ id: "x", workflow_name: "shorts_historical_series" }),
      card({ id: "y", bot_id: "pipeline-board-demo", workflow_name: "pipeline_board_demo" }),
    ];
    const f = { ...emptyPipelineFilters(), bot: "shorts_historical_series" };
    expect(filterPipelineCards(looseCards, f).map((c) => c.id)).toEqual(["x"]);
    // A card whose bot_id is set does NOT also answer to its workflow name —
    // the identity is bot_id first, one value per card.
    const f2 = { ...emptyPipelineFilters(), bot: "pipeline_board_demo" };
    expect(filterPipelineCards(looseCards, f2)).toEqual([]);
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
      ...emptyPipelineFilters(),
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

describe("partitionPipelineCards + inventory filters", () => {
  it("splits in progress from inventory and sorts newest first", () => {
    const mixed = [
      card({
        id: "old-open",
        column_id: "opened",
        updated_at: "2026-07-01T00:00:00Z",
      }),
      card({
        id: "new-closed",
        column_id: "closed",
        updated_at: "2026-07-14T00:00:00Z",
        failed: true,
      }),
      card({
        id: "running",
        column_id: "in_progress",
        status: "running",
        updated_at: "2026-07-10T00:00:00Z",
      }),
    ];
    const { inProgress, inventory } = partitionPipelineCards(mixed);
    expect(inProgress.map((c) => c.id)).toEqual(["running"]);
    expect(inventory.map((c) => c.id)).toEqual(["new-closed", "old-open"]);
  });

  it("filterInventoryCards applies tab + subfilter", () => {
    const inv = [
      card({ id: "r", column_id: "opened", ready: true }),
      card({ id: "d", column_id: "opened" }),
      card({ id: "ok", column_id: "closed", failed: false }),
      card({ id: "bad", column_id: "closed", failed: true }),
    ];
    const base = emptyPipelineFilters();
    expect(
      filterInventoryCards(inv, { ...base, inventoryTab: "opened", openedSubfilter: "ready" }).map(
        (c) => c.id,
      ),
    ).toEqual(["r"]);
    expect(
      filterInventoryCards(inv, {
        ...base,
        inventoryTab: "opened",
        openedSubfilter: "not_ready",
      }).map((c) => c.id),
    ).toEqual(["d"]);
    expect(
      filterInventoryCards(inv, {
        ...base,
        inventoryTab: "closed",
        closedSubfilter: "success",
      }).map((c) => c.id),
    ).toEqual(["ok"]);
    expect(
      filterInventoryCards(inv, {
        ...base,
        inventoryTab: "closed",
        closedSubfilter: "failed",
      }).map((c) => c.id),
    ).toEqual(["bad"]);
    expect(
      filterInventoryCards(inv, { ...base, inventoryTab: "opened" }).map((c) => c.id),
    ).toEqual(["r", "d"]);
  });

  it("sortNewestFirst is stable on equal timestamps via id", () => {
    const got = sortNewestFirst([
      card({ id: "b", updated_at: "2026-07-01T00:00:00Z" }),
      card({ id: "a", updated_at: "2026-07-01T00:00:00Z" }),
    ]);
    expect(got.map((c) => c.id)).toEqual(["a", "b"]);
  });

  it("sortInventoryCards priority matches launch order (P desc, then oldest)", () => {
    const got = sortInventoryCards(
      [
        card({
          id: "old-low",
          priority: 1,
          created_at: "2026-07-01T00:00:00Z",
        }),
        card({
          id: "new-high",
          priority: 5,
          created_at: "2026-07-10T00:00:00Z",
        }),
        card({
          id: "old-mid",
          priority: 3,
          created_at: "2026-07-02T00:00:00Z",
        }),
        card({
          id: "new-mid",
          priority: 3,
          created_at: "2026-07-05T00:00:00Z",
        }),
        card({
          id: "unprioritized",
          priority: 0,
          created_at: "2026-06-01T00:00:00Z",
        }),
      ],
      "priority",
    );
    expect(got.map((c) => c.id)).toEqual([
      "new-high",
      "old-mid",
      "new-mid",
      "old-low",
      "unprioritized",
    ]);
  });

  it("sortInventoryCards updated/created are newest-first", () => {
    const cards = [
      card({
        id: "a",
        created_at: "2026-07-01T00:00:00Z",
        updated_at: "2026-07-10T00:00:00Z",
      }),
      card({
        id: "b",
        created_at: "2026-07-05T00:00:00Z",
        updated_at: "2026-07-02T00:00:00Z",
      }),
    ];
    expect(sortInventoryCards(cards, "updated").map((c) => c.id)).toEqual([
      "a",
      "b",
    ]);
    expect(sortInventoryCards(cards, "created").map((c) => c.id)).toEqual([
      "b",
      "a",
    ]);
  });

  it("emptyPipelineFilters defaults sortMode to priority", () => {
    expect(emptyPipelineFilters().sortMode).toBe("priority");
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
    expect(
      pipelineFiltersActive({ ...emptyPipelineFilters(), openedSubfilter: "ready" }),
    ).toBe(true);
  });

  it("emptyPipelineFilters returns fresh Sets (no aliasing across resets)", () => {
    const a = emptyPipelineFilters();
    const b = emptyPipelineFilters();
    a.labels.add("x");
    expect(b.labels.size).toBe(0);
  });
});
