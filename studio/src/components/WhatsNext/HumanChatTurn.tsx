import { useState } from "react";

import type { HumanQuestionMessage } from "@/lib/whats-next/messages";
import type { FormAnswer, FormSpec } from "@/lib/whats-next/questionForm";
import { Button } from "@/components/ui/Button";
import { Textarea } from "@/components/ui/Textarea";
import { WizardForm } from "@/components/ui/WizardForm";
import { useUIStore } from "@/store/ui";

interface Props {
  message: HumanQuestionMessage;
  // Optional rich form. When present, the chat renders WizardForm
  // and the resulting FormAnswer is forwarded verbatim to the parent
  // (question.id is the answer key). When absent the legacy
  // textarea + optional approve/reject UI is used.
  form?: FormSpec;
  // Called by the user. Wired in Étape 3 — for Étape 1 the parent
  // passes a no-op (or local-state updater for mock progression).
  onSubmit?: (outcome: {
    text: string;
    approved?: boolean;
    formAnswer?: FormAnswer;
  }) => void;
  busy?: boolean;
}

export default function HumanChatTurn({
  message,
  form,
  onSubmit,
  busy = false,
}: Props) {
  const [draft, setDraft] = useState("");
  const [reviseOpen, setReviseOpen] = useState(false);
  const chatEnterSubmits = useUIStore((s) => s.chatEnterSubmits);

  if (message.status === "answered") {
    return <AnsweredTurn message={message} />;
  }

  const hasActions = (message.actions?.length ?? 0) > 0;
  // The rich form takes precedence over both the legacy free-text
  // and the approve/reject UI. When a form is present we ignore
  // textField/approvedField: the form's question.id is the answer
  // key directly.
  const hasForm = !!form && form.questions.length > 0;
  const isFreeText = !hasForm && !hasActions;

  const submit = (approved?: boolean) => {
    if (!onSubmit || busy) return;
    onSubmit({ text: draft, approved });
    setDraft("");
    setReviseOpen(false);
  };

  const submitForm = (formAnswer: FormAnswer) => {
    if (!onSubmit || busy) return;
    onSubmit({ text: "", formAnswer });
  };

  // Shared keybinding for both Textareas (free-text + revise). Honors
  // the global chatEnterSubmits preference. The `onEnter` callback
  // decides what "submit" means in each context (free-text vs revise).
  const makeKeyHandler =
    (onEnter: () => void) =>
    (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
      if (e.key !== "Enter") return;
      if (chatEnterSubmits) {
        if (e.shiftKey) return;
        e.preventDefault();
        onEnter();
      } else {
        if (!(e.metaKey || e.ctrlKey)) return;
        e.preventDefault();
        onEnter();
      }
    };

  return (
    <div className="space-y-2">
      <AssistantBubble text={message.prompt} />

      {hasForm && (
        <div className="ml-6">
          <WizardForm spec={form!} onSubmit={submitForm} busy={busy} />
        </div>
      )}

      {isFreeText && (
        <div className="flex items-stretch gap-2 ml-6">
          <Textarea
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={makeKeyHandler(() => {
              if (draft.trim() !== "") submit();
            })}
            placeholder="Type your answer…"
            rows={Math.max(2, Math.min(10, Math.ceil(draft.length / 60) + 1))}
            disabled={busy}
            className="flex-1"
          />
          <Button
            variant="primary"
            size="sm"
            disabled={busy || draft.trim() === ""}
            onClick={() => submit()}
            className="self-end"
          >
            {busy ? "…" : "Send"}
          </Button>
        </div>
      )}

      {hasActions && (
        <div className="ml-6 space-y-2">
          <div className="flex items-center gap-2">
            <Button
              variant="primary"
              size="sm"
              disabled={busy}
              onClick={() => submit(true)}
            >
              {busy ? "…" : "Approve"}
            </Button>
            <Button
              variant="secondary"
              size="sm"
              disabled={busy}
              onClick={() => setReviseOpen((v) => !v)}
            >
              Request revision
            </Button>
          </div>

          {reviseOpen && (
            <div className="flex items-stretch gap-2">
              <Textarea
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
                onKeyDown={makeKeyHandler(() => {
                  if (draft.trim() !== "") submit(false);
                })}
                placeholder="What should be revised?"
                rows={3}
                disabled={busy}
                className="flex-1"
              />
              <Button
                variant="primary"
                size="sm"
                disabled={busy || draft.trim() === ""}
                onClick={() => submit(false)}
                className="self-end"
              >
                {busy ? "…" : "Send"}
              </Button>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

function AssistantBubble({ text }: { text: string }) {
  return (
    <div className="flex items-start gap-2">
      <span
        className="mt-1 w-5 h-5 rounded-full bg-accent-soft text-accent-fg text-[10px] font-bold flex items-center justify-center shrink-0"
        aria-hidden="true"
      >
        AI
      </span>
      <div className="flex-1 rounded-lg bg-surface-2 border border-border-subtle px-3 py-2 text-[13px] text-fg-default">
        {text}
      </div>
    </div>
  );
}

function AnsweredTurn({ message }: { message: HumanQuestionMessage }) {
  return (
    <div className="space-y-2">
      <AssistantBubble text={message.prompt} />
      <div className="flex items-start gap-2 ml-6">
        <span
          className="mt-1 w-5 h-5 rounded-full bg-surface-3 text-fg-default text-[10px] font-bold flex items-center justify-center shrink-0"
          aria-hidden="true"
        >
          You
        </span>
        <div className="flex-1 rounded-lg bg-surface-1 border border-border-subtle px-3 py-2 text-[13px] text-fg-default whitespace-pre-wrap">
          {message.userReply || (
            <span className="italic text-fg-subtle">
              {message.outcome && "approved" in message.outcome
                ? message.outcome.approved
                  ? "approved"
                  : "requested revision"
                : "(empty reply)"}
            </span>
          )}
        </div>
      </div>
    </div>
  );
}
