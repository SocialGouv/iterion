import { Fragment, useMemo, useState } from "react";
import { Link } from "wouter";

import { resumeRun } from "@/api/runs";
import type { PipelineBoardCard } from "@/api/pipelineBoards";
import HumanPromptForm from "@/components/Runs/conversation/HumanPromptForm";
import type { StagedHumanSubmission } from "@/components/Runs/conversation/HumanPromptForm";
import { errorMessage } from "@/lib/errorHints";
import { ReviewScopePanel } from "./ReviewScopePanel";
import { Badge, Button, InlineBanner } from "@/components/ui";

import {
  clampReviewIndex,
  pendingReviewVersionKey,
  sortPendingReviewsChronologically,
} from "./reviewQueue";

interface Props {
  card: PipelineBoardCard;
  // Refetches the board after the batch was sent. Individual answers stay as
  // local drafts until that deliberate final action.
  onResolved: () => void;
}

type SubmissionResult = { message: string };

const maxReviewBatchSize = 10;

// SequentialReviews presents one answer page at a time, but only sends a
// fan-out's sibling reviews when the operator has finished the whole group.
// A review without runtime fan-out provenance deliberately remains a batch of
// one: unrelated pauses must never be coupled merely because they appeared on
// the same card at the same time.
export function SequentialReviews({ card, onResolved }: Props) {
  const pending = useMemo(
    () => sortPendingReviewsChronologically(card.pending_reviews ?? []),
    [card.pending_reviews],
  );
  const batchKey = pending[0]?.batch_key;
  const reviews = useMemo(
    () => (batchKey
      ? pending.filter((review) => review.batch_key === batchKey).slice(0, maxReviewBatchSize)
      : pending.slice(0, 1)),
    [batchKey, pending],
  );
  const reviewKeys = useMemo(
    () => reviews.map(pendingReviewVersionKey),
    [reviews],
  );
  const [activeReviewKey, setActiveReviewKey] = useState<string | null>(null);
  const [staged, setStaged] = useState<Record<string, StagedHumanSubmission>>({});
  const [results, setResults] = useState<Record<string, SubmissionResult>>({});
  const [sending, setSending] = useState(false);

  // Polling may remove an answered turn and append a newer turn from the same
  // AI. Keep the exact active version mounted while it still exists, so a
  // draft cannot be replaced underneath the operator. Once it disappears,
  // continue from the oldest page in the current fan-out batch.
  let current = activeReviewKey === null ? -1 : reviewKeys.indexOf(activeReviewKey);
  if (current < 0) current = 0;

  if (reviews.length === 0) return null;

  const total = reviews.length;
  const review = reviews[current];
  const reviewKey = reviewKeys[current];
  if (!review || !reviewKey) return null;

  const stagedCount = reviewKeys.filter((key) => staged[key] !== undefined).length;
  const allStaged = stagedCount === total;

  const selectReview = (index: number) => {
    const key = reviewKeys[clampReviewIndex(index, total)];
    if (key) setActiveReviewKey(key);
  };

  const stage = (submission: StagedHumanSubmission) => {
    setStaged((previous) => ({ ...previous, [reviewKey]: submission }));
    setResults((previous) => {
      const next = { ...previous };
      delete next[reviewKey];
      return next;
    });
    const nextUnstaged = reviewKeys.find(
      (key) => key !== reviewKey && staged[key] === undefined,
    );
    if (nextUnstaged) setActiveReviewKey(nextUnstaged);
  };

  const unstage = () => {
    setStaged((previous) => {
      const next = { ...previous };
      delete next[reviewKey];
      return next;
    });
    setResults((previous) => {
      const next = { ...previous };
      delete next[reviewKey];
      return next;
    });
  };

  const sendBatch = async () => {
    if (!allStaged || sending) return;
    setSending(true);
    setResults({});
    const outcomes = await Promise.all(
      reviewKeys.map(async (key) => {
        const submission = staged[key];
        if (!submission) return [key, { message: "This response is missing." }] as const;
        try {
          await resumeRun(submission.runId, {
            answers: submission.answers,
            source: submission.source,
            ...(submission.attachments && submission.attachments.length > 0
              ? { attachments: submission.attachments }
              : {}),
            ...(submission.force ? { force: true } : {}),
          });
          return [key, null] as const;
        } catch (error) {
          return [key, { message: errorMessage(error) }] as const;
        }
      }),
    );
    const failures = Object.fromEntries(
      outcomes.filter(([, result]) => result !== null) as Array<[string, SubmissionResult]>,
    );
    setSending(false);
    if (Object.keys(failures).length === 0) {
      setStaged({});
      onResolved();
      return;
    }
    // Successful resumes must never be sent a second time. A refresh removes
    // their completed cards while retaining the failed drafts and their exact
    // server explanation for correction or a force retry.
    setStaged((previous) =>
      Object.fromEntries(Object.entries(previous).filter(([key]) => failures[key] !== undefined)),
    );
    setResults(failures);
    onResolved();
  };

  return (
    <div className="space-y-2 rounded-md border border-warning/40 bg-warning-soft p-2">
      <div className="flex items-center gap-2">
        <span className="text-micro font-medium uppercase tracking-wide text-warning-fg">
          Awaiting input
        </span>
        {total > 1 && (
          <span className="text-micro text-fg-subtle">
            Review {current + 1} of {total} · {stagedCount} prepared
          </span>
        )}
        {review.depth > 0 && <Badge variant="neutral">child · depth {review.depth}</Badge>}
        {total > 1 && (
          <div className="ml-auto flex items-center gap-1">
            <Button
              variant="secondary"
              size="sm"
              disabled={current <= 0 || sending}
              onClick={() => selectReview(current - 1)}
              aria-label="Previous review"
            >
              Prev
            </Button>
            <Button
              variant="secondary"
              size="sm"
              disabled={current >= total - 1 || sending}
              onClick={() => selectReview(current + 1)}
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

      <Fragment key={reviewKey}>
        {review.run_id && (
          <ReviewScopePanel runId={review.run_id} pauseKey={reviewKey} live />
        )}

        {!review.run_id || !review.node_id ? (
          <InlineBanner tone="warning" layout="inline">
            This pause has no node identifier, so it cannot be answered inline. Open the run
            console to inspect it.
          </InlineBanner>
        ) : staged[reviewKey] ? (
          <div className="flex items-center gap-2 rounded border border-border-subtle bg-surface px-2 py-1">
            <span className="text-micro text-fg-subtle">Response prepared for this review.</span>
            <Button variant="secondary" size="sm" disabled={sending} onClick={unstage}>
              Change response
            </Button>
          </div>
        ) : (
          <HumanPromptForm
            runId={review.run_id}
            nodeId={review.node_id}
            questions={review.questions ?? {}}
            instructions={review.instructions}
            sourceOverride={null}
            onStage={total > 1 ? stage : undefined}
            onResumed={total === 1 ? onResolved : undefined}
            deferSubmission={total > 1}
          />
        )}
      </Fragment>

      {Object.keys(results).length > 0 && (
        <div className="space-y-1" role="alert">
          {Object.entries(results).map(([key, result]) => (
            <p key={key} className="text-danger-fg text-micro">
              One response was not sent: {result.message}
            </p>
          ))}
        </div>
      )}

      {total > 1 && (
        <div className="flex items-center justify-between gap-2 border-t border-border-subtle pt-2">
          <span className="text-micro text-fg-subtle">
            Responses stay editable until the whole batch is sent.
          </span>
          <Button
            variant="primary"
            size="sm"
            disabled={!allStaged || sending}
            loading={sending}
            onClick={() => void sendBatch()}
          >
            Send {total} responses
          </Button>
        </div>
      )}
    </div>
  );
}

export default SequentialReviews;
