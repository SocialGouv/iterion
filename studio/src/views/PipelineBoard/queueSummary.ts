import type { PipelineBoardCard, PipelineConcurrency } from "@/api/pipelineBoards";

import { cardReady } from "./columnFilters";

export interface QueueSummary {
  readyCount: number;
  waitingDepsCount: number;
  draftCount: number;
  /** Free concurrency slots; null when cap disabled. */
  slotsFree: number | null;
  slotsMax: number | null;
  slotsActive: number | null;
  /** Next ticket the admission loop would pick (among ready opened cards). */
  nextUp: PipelineBoardCard | null;
}

function hasOpenDeps(card: PipelineBoardCard): boolean {
  if ((card.open_blocker_count ?? 0) > 0) return true;
  if (card.issue_state === "waiting_deps") return true;
  const reason = card.launch_blocked_reason;
  return (
    reason === "open_blockers" ||
    reason === "waiting_deps" ||
    reason === "blocker_labels"
  );
}

/** Same order as server sortReadyTickets: priority desc, then created_at asc. */
export function sortLaunchOrder(cards: PipelineBoardCard[]): PipelineBoardCard[] {
  return [...cards].sort((a, b) => {
    if (a.priority !== b.priority) return (b.priority ?? 0) - (a.priority ?? 0);
    const ta = Date.parse(a.created_at || "") || 0;
    const tb = Date.parse(b.created_at || "") || 0;
    if (ta !== tb) return ta - tb;
    return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
  });
}

export function computeQueueSummary(
  cards: PipelineBoardCard[],
  concurrency?: PipelineConcurrency,
): QueueSummary {
  let readyCount = 0;
  let waitingDepsCount = 0;
  let draftCount = 0;
  const ready: PipelineBoardCard[] = [];

  for (const card of cards) {
    if (card.column_id !== "opened") continue;
    if (cardReady(card)) {
      readyCount++;
      ready.push(card);
      continue;
    }
    if (hasOpenDeps(card)) {
      waitingDepsCount++;
      continue;
    }
    draftCount++;
  }

  const nextUp = sortLaunchOrder(ready)[0] ?? null;

  let slotsFree: number | null = null;
  let slotsMax: number | null = null;
  let slotsActive: number | null = null;
  if (concurrency?.enabled && concurrency.max > 0) {
    slotsMax = concurrency.max;
    slotsActive = concurrency.active;
    slotsFree = Math.max(0, concurrency.max - concurrency.active);
  }

  return {
    readyCount,
    waitingDepsCount,
    draftCount,
    slotsFree,
    slotsMax,
    slotsActive,
    nextUp,
  };
}

export { hasOpenDeps };
