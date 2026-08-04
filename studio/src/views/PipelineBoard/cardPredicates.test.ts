import { describe, expect, it } from "vitest";

import type { PipelineBoardCard } from "@/api/pipelineBoards";

import {
  cardBlocked,
  cardReady,
  closedOutcome,
  compareBlockedLast,
  compareLaunchOrder,
  isKnownLane,
} from "./cardPredicates";

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

describe("cardBlocked", () => {
  it("is true for every hard-dependency signal the server can send", () => {
    expect(cardBlocked(card({ open_blocker_count: 2 }))).toBe(true);
    expect(cardBlocked(card({ issue_state: "waiting_deps" }))).toBe(true);
    expect(cardBlocked(card({ launch_blocked_reason: "open_blockers" }))).toBe(true);
    expect(cardBlocked(card({ launch_blocked_reason: "waiting_deps" }))).toBe(true);
    // blocker_labels was missing from one of the three former copies, so a
    // label-gated ticket was badged Blocked yet escaped the filter.
    expect(cardBlocked(card({ launch_blocked_reason: "blocker_labels" }))).toBe(true);
  });

  it("is false for reasons that are about the ticket's own preparation", () => {
    expect(cardBlocked(card({}))).toBe(false);
    expect(cardBlocked(card({ launch_blocked_reason: "no_bot" }))).toBe(false);
    expect(cardBlocked(card({ launch_blocked_reason: "not_ready" }))).toBe(false);
    expect(cardBlocked(card({ open_blocker_count: 0 }))).toBe(false);
  });
});

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

describe("compareLaunchOrder", () => {
  it("orders by priority desc, then oldest-created, then id", () => {
    const hi = card({ id: "hi", priority: 9, created_at: "2026-01-02T00:00:00Z" });
    const lo = card({ id: "lo", priority: 1, created_at: "2026-01-01T00:00:00Z" });
    expect(compareLaunchOrder(hi, lo)).toBeLessThan(0);

    const older = card({ id: "a", priority: 5, created_at: "2026-01-01T00:00:00Z" });
    const newer = card({ id: "b", priority: 5, created_at: "2026-01-02T00:00:00Z" });
    expect(compareLaunchOrder(older, newer)).toBeLessThan(0);

    const sameA = card({ id: "a", priority: 5, created_at: "2026-01-01T00:00:00Z" });
    const sameB = card({ id: "b", priority: 5, created_at: "2026-01-01T00:00:00Z" });
    expect(compareLaunchOrder(sameA, sameB)).toBeLessThan(0);
  });

  it("treats an absent priority as 0 and still applies the date tie-break", () => {
    // Regression: the former comparator tested `a.priority !== b.priority`
    // BEFORE defaulting, so `undefined` vs `0` entered the branch, returned
    // `0 - 0 === 0`, and the created_at tie-break was never reached — two
    // cards a day apart compared as equal.
    const noPriority = card({ id: "b", created_at: "2026-01-01T00:00:00Z" });
    const zeroPriority = card({ id: "a", priority: 0, created_at: "2026-01-02T00:00:00Z" });
    expect(compareLaunchOrder(noPriority, zeroPriority)).toBeLessThan(0);
    expect(compareLaunchOrder(zeroPriority, noPriority)).toBeGreaterThan(0);
  });
});

describe("compareBlockedLast", () => {
  it("sinks dependency-blocked cards below launchable ones", () => {
    const free = card({ id: "free" });
    const blocked = card({ id: "blocked", open_blocker_count: 1 });
    expect(compareBlockedLast(free, blocked)).toBeLessThan(0);
    expect(compareBlockedLast(blocked, free)).toBeGreaterThan(0);
    expect(compareBlockedLast(free, card({ id: "other" }))).toBe(0);
    expect(compareBlockedLast(blocked, card({ id: "b2", issue_state: "waiting_deps" }))).toBe(0);
  });
});

describe("isKnownLane", () => {
  it("recognises the four shipped lanes and nothing else", () => {
    for (const lane of ["opened", "in_progress", "needs_attention", "closed"]) {
      expect(isKnownLane(lane)).toBe(true);
    }
    expect(isKnownLane("some_future_lane")).toBe(false);
    expect(isKnownLane(undefined)).toBe(false);
  });
});
