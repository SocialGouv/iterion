import { useState } from "react";
import { Link } from "wouter";

import type { PipelineBoardCard } from "@/api/pipelineBoards";
import HumanPromptForm from "@/components/Runs/conversation/HumanPromptForm";
import { Badge, Button, InlineBanner } from "@/components/ui";

interface Props {
  card: PipelineBoardCard;
  // Refetches the board. Called after every successful answer BEFORE the
  // operator advances, because resuming a review can retire it or spawn new
  // child reviews — the fresh projection is authoritative.
  onResolved: () => void;
}

// clampReviewIndex keeps the active review index inside [0, total). The board
// refetch after each answer can shrink the pending set out from under the
// current index, so every read clamps.
export function clampReviewIndex(index: number, total: number): number {
  if (total <= 0) return 0;
  return Math.min(Math.max(index, 0), total - 1);
}

// SequentialReviews steps through a card's pending human interactions one at a
// time. Each pause can live in the root run or any descendant; the answer is
// POSTed to the exact run_id/node_id the review names (via HumanPromptForm's
// existing resume path — not reimplemented here).
export function SequentialReviews({ card, onResolved }: Props) {
  const reviews = card.pending_reviews ?? [];
  const [index, setIndex] = useState(0);

  if (reviews.length === 0) return null;

  const total = reviews.length;
  const current = clampReviewIndex(index, total);
  const review = reviews[current];
  if (!review) return null;

  return (
    <div className="space-y-2 rounded-md border border-warning/40 bg-warning-soft p-2">
      <div className="flex items-center gap-2">
        <span className="text-micro font-medium uppercase tracking-wide text-warning-fg">
          Awaiting input
        </span>
        {total > 1 && (
          <span className="text-micro text-fg-subtle">
            Review {current + 1} of {total}
          </span>
        )}
        {review.depth > 0 && <Badge variant="neutral">child · depth {review.depth}</Badge>}
        {total > 1 && (
          <div className="ml-auto flex items-center gap-1">
            <Button
              variant="secondary"
              size="sm"
              disabled={current <= 0}
              onClick={() => setIndex(clampReviewIndex(current - 1, total))}
              aria-label="Previous review"
            >
              Prev
            </Button>
            <Button
              variant="secondary"
              size="sm"
              disabled={current >= total - 1}
              onClick={() => setIndex(clampReviewIndex(current + 1, total))}
              aria-label="Next review"
            >
              Next
            </Button>
          </div>
        )}
      </div>

      {(review.workflow_name || review.node_id) && (
        <div className="flex min-w-0 flex-wrap items-center gap-1 text-caption text-fg-subtle">
          {review.run_id && (
            <Link
              href={`/runs/${encodeURIComponent(review.run_id)}`}
              className="font-mono text-accent-text hover:underline"
              title={`Open run ${review.run_id}`}
            >
              {review.run_id.slice(0, 12)}
            </Link>
          )}
          {review.workflow_name && <span className="truncate">{review.workflow_name}</span>}
          {review.node_id && (
            <code className="truncate" title={review.node_id}>
              {review.node_id}
            </code>
          )}
        </div>
      )}

      {review.run_id && review.node_id ? (
        <HumanPromptForm
          key={`${review.run_id}:${review.node_id}`}
          runId={review.run_id}
          nodeId={review.node_id}
          questions={review.questions ?? {}}
          sourceOverride={null}
          onResumed={onResolved}
        />
      ) : (
        <InlineBanner tone="warning" layout="inline">
          This pause has no node identifier, so it cannot be answered inline. Open the run
          console to inspect it.
        </InlineBanner>
      )}
    </div>
  );
}

export default SequentialReviews;
