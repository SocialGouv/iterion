import { useEffect, useState } from "react";

import type {
  HumanQuestionMessage,
  QuickActionKind,
} from "@/lib/whats-next/messages";
import type { FormAnswer, FormSpec } from "@/lib/whats-next/questionForm";
import { Button } from "@/components/ui/Button";
import { Textarea } from "@/components/ui/Textarea";
import { WizardForm } from "@/components/ui/WizardForm";
import { useUIStore } from "@/store/ui";

// Default quick-actions surfaced on every free-text turn unless the
// node overrides via message.quickActions. "later" is intentionally
// omitted from the default — it's only useful inside loops that can
// re-ask (the new triage_loop, eventually), and surfacing it
// everywhere would confuse operators.
const DEFAULT_QUICK_ACTIONS: ReadonlyArray<QuickActionKind> = ["skip", "idk"];

const QUICK_ACTION_LABEL: Record<QuickActionKind, string> = {
  skip: "Skip",
  idk: "I don't know",
  later: "Ask later",
};

const QUICK_ACTION_MARKER: Record<QuickActionKind, string> = {
  skip: "[QA:skip]",
  idk: "[QA:idk]",
  later: "[QA:later]",
};

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
  // Local submitting flag that flips synchronously on click. The parent's
  // `busy` prop only flips after React commits the request; in the gap
  // between the first click and the busy=true commit, a double-click
  // would fire two submits — the second hits the 400 "cannot be
  // resumed (status: running)" race (F-NEW-6). We clear it as soon as
  // the message status advances to "answered" (or the parent reports
  // busy, whichever comes first).
  const [localSubmitting, setLocalSubmitting] = useState(false);
  const chatEnterSubmits = useUIStore((s) => s.chatEnterSubmits);

  // Clear local submitting once the message moves to "answered" — the
  // turn will unmount or re-render in AnsweredTurn, but during the
  // transition we want the button to stay disabled.
  if (localSubmitting && message.status === "answered") {
    // Reset on the next tick to avoid setState-during-render.
    queueMicrotask(() => setLocalSubmitting(false));
  }

  // Also clear localSubmitting when the parent's `busy` flips back to
  // false — that signals "submission completed, success OR failure".
  // Without this, a resumeRun that errored (hash mismatch, schema
  // mismatch, run-terminal-state rejection) would leave the form's
  // submit button permanently disabled: the parent cleared its
  // busyMessageId on the catch + finally, but localSubmitting stayed
  // true because the message.status path never reached "answered".
  useEffect(() => {
    if (!busy && localSubmitting) {
      // Microtask defer keeps this consistent with the message-status
      // clear above (both fire after the current render commits).
      queueMicrotask(() => setLocalSubmitting(false));
    }
  }, [busy, localSubmitting]);

  // The disabled gate consults BOTH busy (parent-tracked async state)
  // and localSubmitting (this-render click state) — either being true
  // is enough to prevent re-entry.
  const disabled = busy || localSubmitting;

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
    if (!onSubmit || disabled) return;
    setLocalSubmitting(true);
    onSubmit({ text: draft, approved });
    setDraft("");
    setReviseOpen(false);
  };

  const submitForm = (formAnswer: FormAnswer) => {
    if (!onSubmit || disabled) return;
    setLocalSubmitting(true);
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
          <WizardForm spec={form!} onSubmit={submitForm} busy={disabled} />
        </div>
      )}

      {isFreeText && (
        <div className="space-y-1 ml-6">
          <div className="flex items-stretch gap-2">
            <Textarea
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              onKeyDown={makeKeyHandler(() => {
                if (draft.trim() !== "") submit();
              })}
              placeholder="Type your answer…"
              rows={Math.max(2, Math.min(10, Math.ceil(draft.length / 60) + 1))}
              disabled={disabled}
              className="flex-1"
            />
            <Button
              variant="primary"
              size="sm"
              disabled={disabled || draft.trim() === ""}
              onClick={() => submit()}
              className="self-end"
            >
              {disabled ? "…" : "Send"}
            </Button>
          </div>
          <QuickActionStrip
            actions={message.quickActions ?? DEFAULT_QUICK_ACTIONS}
            disabled={disabled}
            onPick={(kind) => {
              if (!onSubmit) return;
              // Emit the canonical marker directly — bypass `draft`
              // so a half-typed answer doesn't get concatenated with
              // the marker. The bot's prompt is responsible for
              // recognising [QA:*] and reacting accordingly.
              onSubmit({ text: QUICK_ACTION_MARKER[kind] });
              setDraft("");
              setReviseOpen(false);
            }}
          />
        </div>
      )}

      {hasActions && (
        <div className="ml-6 space-y-2">
          <div className="flex items-center gap-2">
            <Button
              variant="primary"
              size="sm"
              disabled={disabled}
              onClick={() => submit(true)}
            >
              {disabled ? "…" : "Approve"}
            </Button>
            <Button
              variant="secondary"
              size="sm"
              disabled={disabled}
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
                disabled={disabled}
                className="flex-1"
              />
              <Button
                variant="primary"
                size="sm"
                disabled={disabled || draft.trim() === ""}
                onClick={() => submit(false)}
                className="self-end"
              >
                {disabled ? "…" : "Send"}
              </Button>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

function QuickActionStrip({
  actions,
  disabled,
  onPick,
}: {
  actions: ReadonlyArray<QuickActionKind>;
  disabled: boolean;
  onPick: (kind: QuickActionKind) => void;
}) {
  if (actions.length === 0) return null;
  return (
    <div className="flex items-center gap-1 text-[11px] text-fg-subtle">
      <span className="mr-1">Quick:</span>
      {actions.map((kind) => (
        <button
          key={kind}
          type="button"
          disabled={disabled}
          onClick={() => onPick(kind)}
          className="rounded border border-border-subtle bg-surface-1 px-2 py-0.5 hover:bg-surface-2 disabled:opacity-50 disabled:cursor-not-allowed"
          title={`Submit ${QUICK_ACTION_MARKER[kind]} — the bot decides what to do`}
        >
          {QUICK_ACTION_LABEL[kind]}
        </button>
      ))}
    </div>
  );
}

function AssistantBubble({ text }: { text: string }) {
  return (
    <div className="flex items-start gap-2">
      <span
        className="mt-1 px-2 py-0.5 rounded-full bg-accent-soft text-accent-fg text-[10px] font-bold flex items-center justify-center shrink-0"
        aria-hidden="true"
      >
        Niblet
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
          className="mt-1 px-2 py-0.5 rounded-full bg-surface-3 text-fg-default text-[10px] font-bold flex items-center justify-center shrink-0"
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
