import { useEffect, useState, type ReactNode } from "react";

import type {
  HumanQuestionMessage,
  QuickActionKind,
} from "@/lib/whats-next/messages";
import type { FormAnswer, FormSpec } from "@/lib/whats-next/questionForm";
import { Button } from "@/components/ui/Button";
import { Textarea } from "@/components/ui/Textarea";
import { WizardForm } from "@/components/ui/WizardForm";
import MarkdownText from "@/components/Runs/conversation/MarkdownText";

import { OperatorBubble } from "./OperatorBubble";
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
  // When true, this pending turn's input is owned by the page's
  // unified composer: render the assistant bubble (the agent's
  // message must stay in the transcript flow) but no inline
  // textarea/buttons.
  inputHidden?: boolean;
  // Display name of the bot speaking (manifest display_name, e.g. "Copi").
  persona: string;
  // Rendered inside this turn's assistant bubble. Only the turn that produced
  // the thing gets one — see ChatTranscript's `bubbleSlot`.
  bubbleSlot?: ReactNode;
}

export default function HumanChatTurn({
  message,
  form,
  onSubmit,
  busy = false,
  inputHidden = false,
  persona,
  bubbleSlot,
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
  // formDraft holds the latest WizardForm answer so a hybrid turn (rich
  // form + Approve/Reject actions) hands the operator's per-item selection
  // to onSubmit on the action click. Declared with the other hooks so it
  // precedes the answered-turn early return below (rules-of-hooks).
  const [formDraft, setFormDraft] = useState<FormAnswer | null>(null);
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
    return <AnsweredTurn message={message} persona={persona} />;
  }

  const hasActions = (message.actions?.length ?? 0) > 0;
  const hasForm = !!form && form.questions.length > 0;
  // Hybrid layout: when BOTH a rich form AND Approve/Reject actions
  // are present (e.g. whats-next's human_review with the
  // selected_titles checkbox column), the form's draft must travel
  // alongside the approved/rejected verdict — otherwise the
  // operator's per-item selection is dropped on the Approve click.
  // We track the latest WizardForm answer in formDraft and hand it
  // off to onSubmit from the action handlers below.
  const isFreeText = !hasForm && !hasActions;
  const formAndActions = hasForm && hasActions;

  const submit = (approved?: boolean) => {
    if (!onSubmit || disabled) return;
    setLocalSubmitting(true);
    onSubmit({
      text: draft,
      approved,
      // formAndActions: ride along whatever's currently in the
      // WizardForm so a click on Approve carries the checkbox state.
      // Pure-actions turns (no form) pass undefined → the
      // textField/approvedField fallback path in WhatsNextView's
      // onHumanSubmit still wires the answer.
      formAnswer:
        formAndActions && formDraft ? formDraft : undefined,
    });
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

  if (inputHidden) {
    return (
      <div className="space-y-2">
        <AssistantBubble text={message.prompt} persona={persona} slot={bubbleSlot} />
      </div>
    );
  }

  return (
    <div className="space-y-2">
      <AssistantBubble text={message.prompt} persona={persona} slot={bubbleSlot} />

      {hasForm && (
        <div className="ml-6">
          <WizardForm
            spec={form!}
            onSubmit={submitForm}
            // Capture the form draft so the Approve/Reject buttons
            // below can ride along with whatever's currently ticked.
            // Form-only turns (no actions) still call submitForm
            // directly — onAnswerChange is just an observer.
            onAnswerChange={formAndActions ? setFormDraft : undefined}
            busy={disabled}
            mode={form!.mode ?? "flat"}
            // Don't render the form's own Submit when the
            // Approve/Reject buttons own the final verdict — the
            // operator would otherwise see two competing "Send"
            // controls on the same turn.
            hideSubmit={formAndActions}
          />
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
              loading={disabled}
              disabled={disabled || draft.trim() === ""}
              onClick={() => submit()}
              className="self-end"
            >
              {disabled ? "Sending…" : "Send"}
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
              loading={disabled}
              disabled={disabled}
              onClick={() => submit(true)}
            >
              {disabled ? "Sending…" : "Approve"}
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
                loading={disabled}
                disabled={disabled || draft.trim() === ""}
                onClick={() => submit(false)}
                className="self-end"
              >
                {disabled ? "Sending…" : "Send"}
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
    <div className="flex items-center gap-1 text-micro text-fg-subtle">
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

function AssistantBubble({
  text,
  persona,
  slot,
}: {
  text: string;
  persona: string;
  // Rendered INSIDE the bubble, under the text. For an offer this turn made —
  // outside it, the offer reads as chrome and gets missed.
  slot?: ReactNode;
}) {
  return (
    <div className="flex items-start gap-2">
      <span
        className="mt-1 px-2 py-0.5 rounded-full bg-accent-soft text-accent-fg text-caption font-bold flex items-center justify-center shrink-0"
        aria-hidden="true"
      >
        {/* The correspondent's persona (manifest display_name). The dock
            is multi-bot, so this is threaded from the active bot rather
            than fixed: a Copi reply must not be labelled "Nexie". */}
        {persona}
      </span>
      <div className="flex-1 rounded-lg bg-surface-2 border border-border-subtle px-3 py-2 text-label text-fg-default">
        {/* The prose is the message; a pasted `.bot` is evidence behind it.
            Folded, the sentence that explains the workflow is readable without
            scrolling past the workflow itself. */}
        <MarkdownText value={text} size="sm" collapsibleCode />
        {slot}
      </div>
    </div>
  );
}

function AnsweredTurn({
  message,
  persona,
}: {
  message: HumanQuestionMessage;
  persona: string;
}) {
  return (
    <div className="space-y-2">
      <AssistantBubble text={message.prompt} persona={persona} />
      <OperatorBubble
        text={message.userReply ?? ""}
        empty={
          message.outcome && "approved" in message.outcome
            ? message.outcome.approved
              ? "approved"
              : "requested revision"
            : undefined
        }
      />
    </div>
  );
}
