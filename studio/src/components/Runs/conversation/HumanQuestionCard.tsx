import { useNodeLabel } from "@/lib/runChat/useNodeLabel";
import type { HumanQuestionMessage } from "@/lib/runChat/types";

import AsyncQuestionForm from "./AsyncQuestionForm";
import HumanPromptForm from "./HumanPromptForm";
import MarkdownText from "./MarkdownText";
import ReviewMergeCard from "./ReviewMergeCard";

interface Props {
  runId: string;
  message: HumanQuestionMessage;
  // True when the run is currently paused at this message's node and
  // the message status is "pending". Drives whether to render the
  // active form or just the answered bubble.
  isActive: boolean;
}

// HumanQuestionCard renders one turn in the chat:
//   - "answered" → a right-aligned bubble showing the operator's reply
//     and an outcome chip (✓ approved / ✗ rejected).
//   - "pending" with `isActive` → the inline form (textarea +
//     quick-replies, or schema-driven WizardForm).
//   - "pending" without `isActive` → an idle placeholder ("Waiting
//     for run to pause here…"). Shouldn't happen often but covers
//     races between the message arriving and the status flipping.
export default function HumanQuestionCard({ runId, message, isActive }: Props) {
  const nodeLabel = useNodeLabel();
  if (message.status === "answered") {
    return <AnsweredBubble message={message} />;
  }
  // Async question (ADR-081): the run keeps executing — always render
  // the non-blocking answer form while pending, regardless of isActive
  // (there is no pause to be "active" at). message.id IS the
  // interaction ID for async cards.
  if (message.async) {
    return (
      <div className="mt-1 rounded-md border border-accent-emphasis/50 bg-accent-soft/15 px-3 py-2 space-y-2">
        <div className="flex items-center gap-2 text-micro">
          <span className="font-medium text-accent-fg">
            Question — the agent keeps working meanwhile
          </span>
          <span
            className="px-1.5 py-0.5 rounded bg-accent-soft/40 text-fg-default"
            title={message.nodeId}
          >
            {nodeLabel(message.nodeId)}
          </span>
        </div>
        <div className="text-body text-fg-default">
          <MarkdownText value={message.prompt} size="sm" />
        </div>
        <AsyncQuestionForm
          runId={runId}
          interactionId={message.id}
          questions={message.questions}
        />
      </div>
    );
  }
  if (!isActive) {
    return (
      <div className="ml-5 mt-1 text-micro italic text-fg-subtle">
        Waiting for the run to pause at this step…{" "}
        <span
          className="not-italic text-caption text-fg-muted"
          title={message.nodeId}
        >
          {nodeLabel(message.nodeId)}
        </span>
      </div>
    );
  }
  // Guided review-&-merge gate (interaction: review) → the dialogue + merge
  // controls replace the standard reply form.
  if (message.review) {
    return <ReviewMergeCard runId={runId} message={message} />;
  }
  return (
    <div className="mt-1 rounded-md border-2 border-warning bg-warning-soft/20 px-3 py-2 space-y-2">
      <div className="flex items-center gap-2 text-micro">
        <span className="font-medium text-warning-fg">
          Your input unblocks this step
        </span>
        <span
          className="px-1.5 py-0.5 rounded bg-warning-soft/40 text-fg-default"
          title={message.nodeId}
        >
          {nodeLabel(message.nodeId)}
        </span>
      </div>
      <div className="text-body text-fg-default">
        <MarkdownText value={message.prompt} size="sm" />
      </div>
      <HumanPromptForm
        runId={runId}
        nodeId={message.nodeId}
        questions={message.questions ?? {}}
        quickActions={message.quickActions}
      />
    </div>
  );
}

function AnsweredBubble({ message }: { message: HumanQuestionMessage }) {
  const reply = message.userReply?.trim() ?? "";
  const approved =
    message.outcome && typeof message.outcome.approved === "boolean"
      ? (message.outcome.approved as boolean)
      : undefined;
  return (
    <div className="flex justify-end">
      <div className="max-w-[80%] rounded-md bg-accent-soft/60 px-3 py-2 text-body text-fg-default">
        {approved !== undefined && (
          <div
            className={`mb-1 text-micro font-medium ${
              approved ? "text-success-fg" : "text-danger-fg"
            }`}
          >
            {approved ? "✓ Approved" : "✗ Rejected"}
          </div>
        )}
        {reply ? (
          <MarkdownText value={reply} size="sm" />
        ) : (
          <span className="italic text-fg-muted">(no comment)</span>
        )}
      </div>
    </div>
  );
}
