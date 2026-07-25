import { useMemo, useState } from "react";
import { Link } from "wouter";

import type { PipelineBoardCard } from "@/api/pipelineBoards";
import HumanPromptForm from "@/components/Runs/conversation/HumanPromptForm";
import ReviewMergeCard from "@/components/Runs/conversation/ReviewMergeCard";
import { Button, InlineBanner } from "@/components/ui";
import type { HumanQuestionMessage } from "@/lib/runChat/types";

import {
  clampReviewIndex,
  pendingReviewVersionKey,
  sortPendingReviewsChronologically,
} from "./reviewQueue";
import { ReviewInstructions } from "./ReviewInstructions";
import { ReviewMediaRefs } from "./ReviewMediaRefs";
import { ReviewWorkspaceFiles } from "./ReviewWorkspaceFiles";

import {
  clampReviewIndex,
  pendingReviewVersionKey,
  sortPendingReviewsChronologically,
} from "./reviewQueue";

interface Props {
  card: PipelineBoardCard;
  // Refetches the board after every successful answer. The component pins the
  // next existing turn first; the fresh projection may then retire the old
  // review or append a new turn without replacing the active form.
  onResolved: () => void;
}

// SequentialReviews steps through a card's pending human interactions one at a
// time. Each pause can live in the root run or any descendant; the answer is
// POSTed to the exact run_id/node_id the review names. Ordinary pauses reuse
// HumanPromptForm; guided `interaction: review` pauses reuse ReviewMergeCard
// so their reserved dialogue/merge actions stay identical to the run console.
export function SequentialReviews({ card, onResolved }: Props) {
  const reviews = useMemo(
    () => sortPendingReviewsChronologically(card.pending_reviews ?? []),
    [card.pending_reviews],
  );
  const reviewKeys = useMemo(
    () => reviews.map(pendingReviewVersionKey),
    [reviews],
  );
  const [activeReviewKey, setActiveReviewKey] = useState<string | null>(null);

  // Polling may remove an answered turn and append a newer turn from the same
  // AI. Keep the exact active version mounted while it still exists, so a
  // draft in another form cannot be replaced underneath the operator. Once
  // it disappears, continue from the (oldest) head of the refreshed queue.
  let current = activeReviewKey === null ? -1 : reviewKeys.indexOf(activeReviewKey);
  if (current < 0) current = 0;

  if (reviews.length === 0) return null;

  const total = reviews.length;
  const review = reviews[current];
  const reviewKey = reviewKeys[current];
  if (!review || !reviewKey) return null;

  const selectReview = (index: number) => {
    const key = reviewKeys[clampReviewIndex(index, total)];
    if (key) setActiveReviewKey(key);
  };

  const handleResolved = () => {
    // Advance immediately to the oldest other pending turn and pin it before
    // the refetch returns. A new turn from the just-answered AI can then only
    // append behind this active form; it cannot steal the screen.
    setActiveReviewKey(reviewKeys.find((key) => key !== reviewKey) ?? null);
    onResolved();
  };

  return (
    <div className="max-w-5xl space-y-4 rounded-lg border border-warning/50 bg-warning-soft/40 p-4">
      <div className="flex items-center gap-2">
        <span className="text-body font-medium uppercase tracking-wide text-warning-fg">
          Awaiting input
        </span>
        {total > 1 && (
          <span className="text-body text-fg-subtle">
            Review {current + 1} of {total}
          </span>
        )}
        {total > 1 && (
          <div className="ml-auto flex items-center gap-1">
            <Button
              variant="secondary"
              size="sm"
              disabled={current <= 0}
              onClick={() => selectReview(current - 1)}
              aria-label="Previous review"
            >
              Prev
            </Button>
            <Button
              variant="secondary"
              size="sm"
              disabled={current >= total - 1}
              onClick={() => selectReview(current + 1)}
              aria-label="Next review"
            >
              Next
            </Button>
          </div>
        )}
      </div>

      <ReviewWorkspaceFiles
        key={`workspace-files:${reviewKey}`}
        questions={review.questions}
        runId={review.run_id}
        instructions={review.instructions}
      />

      {(review.media?.length ?? 0) > 0 && (
        <ReviewMediaRefs
          key={`media:${reviewKey}`}
          media={review.media ?? []}
          fallbackRunId={review.run_id}
        />
      )}

      {!guidedMessage && (review.instructions || hasAIReviewContent) && (
        <ReviewInstructions
          instructions={review.instructions ?? ""}
          reviewBrief={review.review_brief}
          questions={review.questions}
        />
      )}

      {review.run_id && review.node_id && guidedMessage ? (
        <ReviewMergeCard
          key={reviewKey}
          runId={review.run_id}
          message={guidedMessage}
          sourceOverride={null}
          onResumed={handleResolved}
        />
      ) : review.run_id && review.node_id ? (
        <HumanPromptForm
          key={reviewKey}
          runId={review.run_id}
          nodeId={review.node_id}
          questions={review.questions ?? {}}
          sourceOverride={null}
          onResumed={handleResolved}
        />
      ) : (
        <InlineBanner tone="warning" layout="inline">
          This pause has no node identifier, so it cannot be answered inline. Open the run
          console to inspect it.
        </InlineBanner>
      )}

      {(review.workflow_name || review.node_id || review.run_id) && (
        <details className="border-t border-border-subtle pt-3 text-body text-fg-subtle">
          <summary className="cursor-pointer font-medium hover:text-fg-default">
            Technical details
          </summary>
          <div className="mt-2 flex min-w-0 flex-wrap items-center gap-2">
            {review.run_id && (
              <Link
                href={`/runs/${encodeURIComponent(review.run_id)}`}
                className="font-mono text-accent-text hover:underline"
                title={`Open run ${review.run_id}`}
              >
                Run {review.run_id.slice(0, 12)}
              </Link>
            )}
            {review.workflow_name && <span className="truncate">{review.workflow_name}</span>}
            {review.node_id && (
              <code className="truncate" title={review.node_id}>
                {review.node_id}
              </code>
            )}
            {review.depth > 0 && <span>child · depth {review.depth}</span>}
          </div>
        </details>
      )}
    </div>
  );
}

export default SequentialReviews;
