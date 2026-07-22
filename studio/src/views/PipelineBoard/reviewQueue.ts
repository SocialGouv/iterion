import type { PipelineBoardPendingReview } from "@/api/pipelineBoards";

// Keeps navigation inside [0, total). Polling can shrink the queue between a
// click and the render that consumes it.
export function clampReviewIndex(index: number, total: number): number {
  if (total <= 0) return 0;
  return Math.min(Math.max(index, 0), total - 1);
}

// Review gates reuse their interaction ID (and often the same run/node) when
// the AI answers and asks another question. The enqueue timestamp therefore
// belongs in the identity: A@10 and the later A@13 are distinct queue turns.
export function pendingReviewVersionKey(review: PipelineBoardPendingReview): string {
  return JSON.stringify([
    review.run_id,
    review.node_id ?? "",
    review.interaction_id ?? "",
    review.updated_at,
  ]);
}

function reviewTimestamp(review: PipelineBoardPendingReview): number {
  const stamp = Date.parse(review.updated_at);
  return Number.isNaN(stamp) ? 0 : stamp;
}

// Oldest pending turn first. The server emits this order too; sorting at the
// component boundary keeps the queue correct for a stale/mixed-version API.
export function sortPendingReviewsChronologically(
  reviews: readonly PipelineBoardPendingReview[],
): PipelineBoardPendingReview[] {
  return [...reviews].sort((a, b) => {
    const timeDelta = reviewTimestamp(a) - reviewTimestamp(b);
    if (timeDelta !== 0) return timeDelta;
    const ka = pendingReviewVersionKey(a);
    const kb = pendingReviewVersionKey(b);
    return ka < kb ? -1 : ka > kb ? 1 : 0;
  });
}
