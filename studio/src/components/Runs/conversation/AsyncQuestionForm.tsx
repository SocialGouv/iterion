import { useState } from "react";

import { answerInteraction } from "@/api/runs";
import { Button } from "@/components/ui/Button";
import {
  askUserAllowsFreeText,
  askUserOptions,
} from "@/lib/askUserOptions";
import { errorMessage } from "@/lib/errorHints";

interface Props {
  runId: string;
  interactionId: string;
  questions?: Record<string, unknown>;
}

// AsyncQuestionForm answers a NON-BLOCKING agent question (ADR-081,
// ask_user_async): the run keeps executing while this card is pending,
// so unlike HumanPromptForm there is no resume/WS-reconnect machinery —
// the answer is queued into the agent's message inbox and the
// interaction_answered event flips the card.
export default function AsyncQuestionForm({ runId, interactionId, questions }: Props) {
  const [text, setText] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const options = askUserOptions(questions);
  const allowFreeText = askUserAllowsFreeText(questions);

  const submit = async (answer: string) => {
    if (!answer.trim() || submitting) return;
    setSubmitting(true);
    setError(null);
    try {
      await answerInteraction(runId, interactionId, answer.trim());
      // No local state flip needed: the interaction_answered event
      // arrives over the live WS and re-renders the card as answered.
    } catch (err) {
      setError(errorMessage(err));
      setSubmitting(false);
    }
  };

  return (
    <div className="space-y-2">
      {options.length > 0 && (
        <div className="flex flex-wrap gap-1.5">
          {options.map((o) => (
            <Button
              key={o.id}
              size="sm"
              variant="secondary"
              disabled={submitting}
              onClick={() => void submit(o.id)}
            >
              {o.label}
            </Button>
          ))}
        </div>
      )}
      {allowFreeText && (
        <form
          className="flex items-end gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            void submit(text);
          }}
        >
          <textarea
            className="min-h-[38px] flex-1 resize-y rounded-md border border-border-default bg-surface-default px-2 py-1.5 text-body text-fg-default placeholder:text-fg-subtle focus:outline-none focus:ring-1 focus:ring-accent-emphasis"
            rows={1}
            placeholder="Answer whenever you're ready — the agent keeps working…"
            value={text}
            disabled={submitting}
            onChange={(e) => setText(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && !e.shiftKey) {
                e.preventDefault();
                void submit(text);
              }
            }}
          />
          <Button type="submit" size="sm" disabled={submitting || !text.trim()}>
            {submitting ? "Sending…" : "Answer"}
          </Button>
        </form>
      )}
      {error && <div className="text-micro text-danger-fg">{error}</div>}
    </div>
  );
}
