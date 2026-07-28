import { errorMessage } from "@/lib/errorHints";
import { useEffect, useRef, useState } from "react";

import { getRun, resumeRun } from "@/api/runs";
import { Button } from "@/components/ui/Button";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Select } from "@/components/ui/Select";
import type { HumanQuestionMessage, ReviewTurn } from "@/lib/runChat/types";
import { useDocumentStore } from "@/store/document";
import { useRunStore } from "@/store/run";

import MarkdownText from "./MarkdownText";

interface Props {
  runId: string;
  message: HumanQuestionMessage;
  // Board callers answer a persisted run rather than the editor buffer. An
  // explicit null therefore suppresses `source`, matching HumanPromptForm.
  sourceOverride?: string | null;
  // When supplied, the caller owns refresh/navigation after resume (the
  // pipeline board refetches its projection instead of reconnecting run WS).
  onResumed?: () => void;
}

// Reserved answer keys — must match pkg/runtime/review.go.
const ACTION_KEY = "__review_action";
const REPLY_KEY = "__review_reply";
const MESSAGE_KEY = "__review_message";
const STRATEGY_KEY = "__review_merge_strategy";

// ReviewMergeCard renders a guided review-&-merge gate (interaction: review):
// the companion↔human dialogue thread, an optional "open review env" link,
// a reply box to continue the conversation, and the squash-merge controls
// (Approve & merge / Force-merge / Request changes). Every action resumes the
// run with a reserved __review_action — approve/force squash-merge the
// worktree during the pause; reply continues the dialogue (re-pauses).
export default function ReviewMergeCard(props: Props) {
  // A new review-turn id is a new form. Keying the stateful body resets its
  // draft/submission state without a synchronous setState effect.
  return <ReviewMergeCardTurn key={props.message.id} {...props} />;
}

function ReviewMergeCardTurn({
  runId,
  message,
  sourceOverride,
  onResumed,
}: Props) {
  const review = message.review;
  const setRunStatus = useRunStore((s) => s.setRunStatus);
  const requestWsReconnect = useRunStore((s) => s.requestWsReconnect);
  const applySnapshot = useRunStore((s) => s.applySnapshot);
  const resyncEventsAfterResume = useRunStore(
    (s) => s.resyncEventsAfterResume,
  );
  const currentSource = useDocumentStore((s) => s.currentSource);
  const resolvedSource =
    (sourceOverride !== undefined ? sourceOverride : currentSource) ?? undefined;

  const [busy, setBusy] = useState(false);
  const [submitted, setSubmitted] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [reply, setReply] = useState("");
  const [strategy, setStrategy] = useState(review?.mergeStrategy ?? "squash");
  const [commitMsg, setCommitMsg] = useState("");

  const snapshotTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(
    () => () => {
      if (snapshotTimerRef.current != null) clearTimeout(snapshotTimerRef.current);
    },
    [],
  );

  if (!review || submitted) return null;

  // Same post-resume re-sync dance as HumanPromptForm: the broker dropped
  // this run's subscribers at the pause, so redial + re-pull events, with a
  // REST snapshot fallback for very short resumes.
  const resume = async (answers: Record<string, unknown>) => {
    setBusy(true);
    setError(null);
    try {
      await resumeRun(runId, { answers, source: resolvedSource });
      setSubmitted(true);
      if (onResumed) {
        onResumed();
        return;
      }
      setRunStatus("running");
      requestWsReconnect();
      resyncEventsAfterResume(runId);
      if (snapshotTimerRef.current != null) clearTimeout(snapshotTimerRef.current);
      snapshotTimerRef.current = setTimeout(() => {
        snapshotTimerRef.current = null;
        // Snapshot refresh failure here means the WS reconnect hasn't
        // landed yet — the next event will redrive the store, so we
        // surface a soft notice rather than block the card.
        getRun(runId)
          .then(applySnapshot)
          .catch((e: unknown) => {
            setError(
              `Snapshot refresh failed: ${(e as Error).message}. The card is up to date with the run's next event.`,
            );
          });
      }, 600);
    } catch (e) {
      setError(errorMessage(e));
      setBusy(false); // keep the card interactive on failure
    }
  };

  const sendReply = () =>
    resume({ [ACTION_KEY]: "reply", [REPLY_KEY]: reply });
  const merge = (action: "approve_merge" | "force_merge") =>
    resume({
      [ACTION_KEY]: action,
      [STRATEGY_KEY]: strategy,
      ...(commitMsg.trim() ? { [MESSAGE_KEY]: commitMsg } : {}),
    });
  const requestChanges = () => resume({ [ACTION_KEY]: "request_changes" });

  const turns = review.turns ?? [];
  const noMerge = review.mergeInto === "none";

  return (
    <div className="mt-1 max-w-5xl space-y-4 rounded-lg border-2 border-warning bg-warning-soft/20 p-4">
      <div className="flex flex-wrap items-center gap-2 text-body">
        <span className="text-label font-medium text-warning-fg">
          Review &amp; merge — test the change, then ship it
        </span>
        <code className="px-1.5 py-0.5 rounded bg-warning-soft/40 font-mono text-fg-default">
          {message.nodeId}
        </code>
        <span className="text-fg-muted">
          turn {turns.length}
          {review.maxTurns > 0 ? `/${review.maxTurns}` : ""}
        </span>
        {review.posture === "agent_verdict_ok" && (
          <span className="rounded bg-accent-soft/50 px-1.5 py-0.5 text-fg-muted">
            agent verdict may auto-merge
          </span>
        )}
        <VerdictChip verdict={review.verdict} />
      </div>

      {review.reviewUrl && (
        <a
          href={review.reviewUrl}
          target="_blank"
          rel="noreferrer"
          className="inline-flex items-center gap-1 text-title text-accent-fg underline"
        >
          ↗ Open review environment ({review.reviewUrl})
        </a>
      )}

      <DialogueThread turns={turns} fallback={message.prompt} />

      {/* Continue the dialogue */}
      <div className="space-y-1">
        <textarea
          className="w-full rounded border border-border-default bg-surface-0 px-3 py-2 text-label text-fg-default"
          rows={3}
          placeholder="Reply to the reviewer (e.g. what you tested, what you saw)…"
          value={reply}
          onChange={(e) => setReply(e.target.value)}
          disabled={busy}
        />
        <Button
          size="sm"
          variant="secondary"
          onClick={sendReply}
          disabled={busy || reply.trim().length === 0}
        >
          Send reply
        </Button>
      </div>

      {/* Merge controls */}
      <div className="space-y-2 border-t border-border-default pt-2">
        {!noMerge && (
          <div className="flex items-center gap-2 text-body">
            <label htmlFor="review-merge-strategy" className="text-fg-muted">
              Strategy
            </label>
            <Select
              id="review-merge-strategy"
              aria-label="Merge strategy"
              value={strategy}
              onChange={(e) => setStrategy(e.target.value)}
              disabled={busy}
              className="w-auto"
            >
              <option value="squash">Squash and merge</option>
              <option value="merge">Merge commit (FF)</option>
            </Select>
            <span className="text-fg-subtle">
              → {review.mergeInto === "current" ? "current branch" : review.mergeInto}
            </span>
          </div>
        )}
        {!noMerge && strategy === "squash" && (
          <textarea
            className="w-full rounded border border-border-default bg-surface-0 px-3 py-2 text-body font-mono text-fg-default"
            rows={2}
            placeholder="(optional) squash commit message — defaults to the run's commits"
            value={commitMsg}
            onChange={(e) => setCommitMsg(e.target.value)}
            disabled={busy}
          />
        )}
        <div className="flex flex-wrap gap-2">
          <Button size="sm" variant="primary" onClick={() => merge("approve_merge")} disabled={busy}>
            {noMerge ? "Approve" : "Approve & merge"}
          </Button>
          <Button size="sm" variant="ghost" onClick={() => merge("force_merge")} disabled={busy}>
            Force-merge
          </Button>
          <Button size="sm" variant="ghost" onClick={requestChanges} disabled={busy}>
            Request changes
          </Button>
        </div>
        <p className="text-body text-fg-subtle">
          Force-merge skips the reviewer&apos;s verdict (git safety checks still
          apply). Request changes returns the run to the implementer.
        </p>
      </div>

      {error && (
        <InlineBanner tone="danger" layout="inline">
          {error}
        </InlineBanner>
      )}
    </div>
  );
}

function DialogueThread({
  turns,
  fallback,
}: {
  turns: ReadonlyArray<ReviewTurn>;
  fallback: string;
}) {
  if (turns.length === 0) {
    return (
      <div className="text-body text-fg-default">
        <MarkdownText value={fallback} size="lg" />
      </div>
    );
  }
  return (
    <div className="space-y-2">
      {turns.map((t, i) =>
        t.role === "human" ? (
          <div key={i} className="flex justify-end">
            <div className="max-w-[80%] rounded-md bg-accent-soft/60 px-3 py-2 text-label leading-relaxed text-fg-default whitespace-pre-wrap break-words">
              {t.content?.trim() || (
                <span className="italic text-fg-muted">(no comment)</span>
              )}
            </div>
          </div>
        ) : (
          <div key={i} className="text-body text-fg-default">
            <MarkdownText value={t.content ?? ""} size="lg" />
          </div>
        ),
      )}
    </div>
  );
}

function VerdictChip({ verdict }: { verdict?: Record<string, unknown> }) {
  if (!verdict) return null;
  const decision = typeof verdict.decision === "string" ? verdict.decision : "";
  if (!decision) return null;
  const approved = decision === "approved";
  const confidence =
    typeof verdict.confidence === "string" ? ` (${verdict.confidence})` : "";
  return (
    <span
      className={`rounded px-1.5 py-0.5 text-caption font-medium ${
        approved ? "text-success-fg" : "text-danger-fg"
      }`}
    >
      {approved ? "✓ approved" : "✗ changes requested"}
      {confidence}
    </span>
  );
}
