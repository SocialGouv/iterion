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
  });

  it("does not apply the dependency filter board-wide", () => {
    // Regression: the deps filter used to live here, so switching it on
    // emptied the In-progress section as a side effect — running cards carry
    // no dependency fields at all (attachDeps only runs for ticket cards).
    // It belongs to the Opened tab; see filterInventoryCards.
    const mixed = [
      card({ id: "running", column_id: "in_progress", status: "running" }),
      card({ id: "blocked", column_id: "opened", open_blocker_count: 1 }),
    ];
    const f = { ...emptyPipelineFilters(), depsFilter: "blocked" as const };
    expect(filterPipelineCards(mixed, f).map((c) => c.id)).toEqual([
      "running",
      "blocked",
    ]);
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

describe("needs-attention lane + dependency filter", () => {
  it("partitions needs_attention into its own bucket, out of the inventory", () => {
    const cards = [
      card({ id: "live", column_id: "in_progress" }),
      card({ id: "broken", column_id: "needs_attention", failed: true }),
      card({ id: "todo", column_id: "opened" }),
      card({ id: "done", column_id: "closed" }),
    ];
    const { inProgress, needsAttention, inventory } = partitionPipelineCards(cards);
    expect(inProgress.map((c) => c.id)).toEqual(["live"]);
    expect(needsAttention.map((c) => c.id)).toEqual(["broken"]);
    // Critically NOT in the inventory: a failed pipeline must not reappear
    // in the Opened queue, where it would look launchable.
    expect(inventory.map((c) => c.id).sort()).toEqual(["done", "todo"]);
  });

  it("routes a lane this build does not know into the Closed tab", () => {
    // Forward compatibility: a newer server can add a lane, and an SPA
    // bundle already loaded in a browser tab cannot be retro-fixed. Falling
    // into Closed is recoverable; vanishing from both tabs is not.
    const cards = [card({ id: "future", column_id: "some_future_lane" })];
    const base = emptyPipelineFilters();
    expect(
      filterInventoryCards(cards, { ...base, inventoryTab: "closed" }).map((c) => c.id),
    ).toEqual(["future"]);
    expect(filterInventoryCards(cards, base)).toEqual([]);
  });

  it("depsFilter narrows the Opened tab three ways", () => {
    const cards = [
      card({ id: "free", column_id: "opened", ready: true }),
      card({ id: "blocked", column_id: "opened", ready: true, open_blocker_count: 1 }),
      card({ id: "labelled", column_id: "opened", launch_blocked_reason: "blocker_labels" }),
    ];
    const base = emptyPipelineFilters();
    expect(filterInventoryCards(cards, base).map((c) => c.id)).toEqual([
      "free",
      "blocked",
      "labelled",
    ]);
    expect(
      filterInventoryCards(cards, { ...base, depsFilter: "unblocked" }).map((c) => c.id),
    ).toEqual(["free"]);
    expect(
      filterInventoryCards(cards, { ...base, depsFilter: "blocked" }).map((c) => c.id),
    ).toEqual(["blocked", "labelled"]);
  });

  it("defaults to no dependency filter and reports it as active once set", () => {
    expect(emptyPipelineFilters().depsFilter).toBe("all");
    expect(pipelineFiltersActive(emptyPipelineFilters())).toBe(false);
    expect(
      pipelineFiltersActive({ ...emptyPipelineFilters(), depsFilter: "unblocked" }),
    ).toBe(true);
  });

  it("sorts blocked tickets below launchable ones before applying priority", () => {
    const cards = [
      card({ id: "blockedP9", column_id: "opened", priority: 9, open_blocker_count: 1 }),
      card({ id: "freeP1", column_id: "opened", priority: 1 }),
      card({ id: "freeP5", column_id: "opened", priority: 5 }),
    ];
    // Opened + priority: the top of the list is always something startable.
    expect(sortInventoryCards(cards, "priority", "opened").map((c) => c.id)).toEqual([
      "freeP5",
      "freeP1",
      "blockedP9",
    ]);
    // Closed history is pure ranking — a terminal ticket keeps whatever
    // blockers it had, and reshuffling history for them is noise.
    expect(sortInventoryCards(cards, "priority", "closed").map((c) => c.id)).toEqual([
      "blockedP9",
      "freeP5",
      "freeP1",
    ]);
    // Date modes are chronology; the operator asked for it explicitly.
    expect(sortInventoryCards(cards, "created", "opened").map((c) => c.id)).toEqual([
      "blockedP9",
      "freeP1",
      "freeP5",
    ]);
  });
});
