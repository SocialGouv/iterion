import type { HumanQuestionMessage } from "@/lib/runChat/types";

import HumanPromptForm from "./HumanPromptForm";

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
  if (message.status === "answered") {
    return <AnsweredBubble message={message} />;
  }
  if (!isActive) {
    return (
      <div className="ml-5 mt-1 text-[11px] italic text-fg-subtle">
        Waiting for run to pause at {message.nodeId}…
      </div>
    );
  }
  return (
    <div className="mt-1 rounded-md border-2 border-warning bg-warning-soft/20 px-3 py-2 space-y-2">
      <div className="flex items-center gap-2 text-[11px]">
        <span className="font-medium text-warning-fg">Paused — needs your input</span>
        <code className="px-1.5 py-0.5 rounded bg-warning-soft/40 font-mono text-fg-default">
          {message.nodeId}
        </code>
      </div>
      <p className="text-[12px] text-fg-default">{message.prompt}</p>
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
      <div className="max-w-[80%] rounded-md bg-accent-soft/60 px-3 py-2 text-[12px] text-fg-default whitespace-pre-wrap break-words">
        {approved !== undefined && (
          <div
            className={`mb-1 text-[11px] font-medium ${
              approved ? "text-success-fg" : "text-danger-fg"
            }`}
          >
            {approved ? "✓ Approved" : "✗ Rejected"}
          </div>
        )}
        {reply || (
          <span className="italic text-fg-muted">(no comment)</span>
        )}
      </div>
    </div>
  );
}
