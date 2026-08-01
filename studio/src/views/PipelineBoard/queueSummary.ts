import type { PipelineBoardCard, PipelineConcurrency } from "@/api/pipelineBoards";

import { cardBlocked, cardReady, compareLaunchOrder } from "./cardPredicates";

export interface QueueSummary {
  readyCount: number;
  waitingDepsCount: number;
  draftCount: number;
  /** Free concurrency slots; null when cap disabled. */
  slotsFree: number | null;
  slotsMax: number | null;
  slotsActive: number | null;
  /**
   * Slots held open by needs-attention pipelines so they can restart into
   * their own budget. No process is running against them. Null when the cap
   * is disabled.
   */
  slotsReserved: number | null;
  /** Next ticket the admission loop would pick (among ready opened cards). */
  nextUp: PipelineBoardCard | null;
}

/** Same order as the server's launch order: priority desc, then created asc. */
export function sortLaunchOrder(cards: PipelineBoardCard[]): PipelineBoardCard[] {
  return [...cards].sort(compareLaunchOrder);
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
    if (cardBlocked(card)) {
      waitingDepsCount++;
      continue;
    }
    draftCount++;
  }

  const nextUp = sortLaunchOrder(ready)[0] ?? null;

  let slotsFree: number | null = null;
  let slotsMax: number | null = null;
  let slotsActive: number | null = null;
  let slotsReserved: number | null = null;
  if (concurrency?.enabled && concurrency.max > 0) {
    slotsMax = concurrency.max;
    slotsActive = concurrency.active;
    slotsReserved = concurrency.reserved ?? 0;
    // Reserved slots are unavailable even though nothing is running in them.
    // Leaving them out of this subtraction is what makes a fully-reserved
    // board read as a hang: "1 free slot" next to a queue that never moves.
    slotsFree = Math.max(0, concurrency.max - concurrency.active - slotsReserved);
  }

  return {
    readyCount,
    waitingDepsCount,
    draftCount,
    slotsFree,
    slotsMax,
    slotsActive,
    slotsReserved,
    nextUp,
  };
}
