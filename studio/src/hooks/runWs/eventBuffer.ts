// Microtask event coalescing. Replay (from_seq=0) on a long run can
// dump hundreds–thousands of envelopes onto the message queue
// back-to-back; committing them one-by-one through `applyEvent`
// triggered an O(N²) array spread plus a re-render per event. Batching
// turns that into a single state mutation per JS task.

import type { RunEvent } from "@/api/runs";

export interface EventBuffer {
  // Buffer an event; schedules a flush on the next microtask.
  queue: (evt: RunEvent) => void;
  // Drain synchronously (no-op when empty). Called before snapshot
  // swaps / bulk batches so seq order is preserved, and on teardown so
  // buffered events aren't lost when React unmounts before the
  // microtask fires.
  flush: () => void;
}

export function createEventBuffer(
  apply: (events: RunEvent[]) => void,
): EventBuffer {
  const buffer: RunEvent[] = [];
  let flushScheduled = false;
  const flush = () => {
    flushScheduled = false;
    if (buffer.length === 0) return;
    const drained = buffer.splice(0, buffer.length);
    apply(drained);
  };
  const queue = (evt: RunEvent) => {
    buffer.push(evt);
    if (!flushScheduled) {
      flushScheduled = true;
      queueMicrotask(flush);
    }
  };
  return { queue, flush };
}
