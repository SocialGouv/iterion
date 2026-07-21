import { describe, expect, it } from "vitest";

import type { PipelineBoardCard } from "@/api/pipelineBoards";

import {
  applyColumnFilter,
  cardReady,
  closedOutcome,
  columnFilterActive,
  emptyColumnFilters,
} from "./columnFilters";

function card(partial: Partial<PipelineBoardCard>): PipelineBoardCard {
  return {
    id: "card",
    kind: "run",
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

describe("cardReady", () => {
  it("is true for staged tasks and queued runs when deps are clear", () => {
    expect(cardReady(card({ ready: true }))).toBe(true);
    expect(cardReady(card({ status: "queued" }))).toBe(true);
    expect(cardReady(card({ kind: "task", ready: false }))).toBe(false);
    expect(cardReady(card({ status: "running" }))).toBe(false);
    expect(cardReady(card({}))).toBe(false);
  });

  it("is false when hard deps still block launch", () => {
    expect(cardReady(card({ ready: true, open_blocker_count: 2 }))).toBe(false);
    expect(cardReady(card({ ready: true, issue_state: "waiting_deps" }))).toBe(false);
    expect(
      cardReady(card({ ready: true, launch_blocked_reason: "open_blockers" })),
    ).toBe(false);
  });
});

describe("closedOutcome", () => {
  it("splits failed from success on the failed flag", () => {
    expect(closedOutcome(card({ failed: true }))).toBe("failed");
    expect(closedOutcome(card({ status: "finished" }))).toBe("success");
    expect(closedOutcome(card({ failed: false }))).toBe("success");
  });
});

describe("emptyColumnFilters", () => {
  it("defaults both lanes to 'all' and is a fresh object each call", () => {
    const a = emptyColumnFilters();
    const b = emptyColumnFilters();
    expect(a).toEqual({ todoReadiness: "all", closedOutcome: "all" });
    a.todoReadiness = "ready";
    expect(b.todoReadiness).toBe("all"); // no aliasing
  });
});

describe("columnFilterActive", () => {
  it("reports a non-default per-column filter", () => {
    const base = emptyColumnFilters();
    expect(columnFilterActive("opened", base)).toBe(false);
    expect(columnFilterActive("opened", { ...base, todoReadiness: "ready" })).toBe(true);
    expect(columnFilterActive("closed", base)).toBe(false);
    expect(columnFilterActive("closed", { ...base, closedOutcome: "failed" })).toBe(true);
    // In progress (and any lane without a control) is never "active".
    expect(columnFilterActive("in_progress", { ...base, todoReadiness: "ready" })).toBe(false);
  });
});

describe("applyColumnFilter", () => {
  const todoCards = [
    card({ id: "ready", ready: true }),
    card({ id: "queued", kind: "run", status: "queued" }),
    card({ id: "draft", kind: "task" }),
  ];
  const closedCards = [
    card({ id: "ok", column_id: "closed", status: "finished" }),
    card({ id: "bad", column_id: "closed", failed: true }),
  ];

  it("Todo: 'ready' keeps cleared-to-launch cards, 'draft' keeps the rest", () => {
    const base = emptyColumnFilters();
    expect(
      applyColumnFilter("opened", todoCards, { ...base, todoReadiness: "ready" }).map((c) => c.id),
    ).toEqual(["ready", "queued"]);
    expect(
      applyColumnFilter("opened", todoCards, { ...base, todoReadiness: "draft" }).map((c) => c.id),
    ).toEqual(["draft"]);
    // 'all' returns the input unchanged (same reference — no needless copy).
    expect(applyColumnFilter("opened", todoCards, base)).toBe(todoCards);
  });

  it("Closed: 'success' and 'failed' narrow by outcome", () => {
    const base = emptyColumnFilters();
    expect(
      applyColumnFilter("closed", closedCards, { ...base, closedOutcome: "success" }).map((c) => c.id),
    ).toEqual(["ok"]);
    expect(
      applyColumnFilter("closed", closedCards, { ...base, closedOutcome: "failed" }).map((c) => c.id),
    ).toEqual(["bad"]);
    expect(applyColumnFilter("closed", closedCards, base)).toBe(closedCards);
  });

  it("a column without a control is returned unchanged", () => {
    const cards = [card({ column_id: "in_progress", status: "running" })];
    expect(
      applyColumnFilter("in_progress", cards, { todoReadiness: "ready", closedOutcome: "failed" }),
    ).toBe(cards);
  });
});
