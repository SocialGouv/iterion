import { useCallback, useEffect, useMemo, useState } from "react";

import { Button } from "@/components/ui/Button";
import { Tabs, type TabItem } from "@/components/ui/Tabs";
import QuestionInput from "@/components/WhatsNext/QuestionInput";
import type {
  FormAnswer,
  FormQuestion,
  FormSpec,
} from "@/lib/whats-next/questionForm";
import { OTHER_SENTINEL } from "@/lib/whats-next/questionForm";

export type WizardMode = "auto" | "wizard" | "flat";

export interface WizardFormProps {
  spec: FormSpec;
  onSubmit: (answers: FormAnswer) => void;
  // Optional change callback. Fires every time the user edits any
  // question's answer (debounced by React's state batching). Lets a
  // parent observe the in-progress draft without waiting for the
  // atomic submit — used by RunView's Approve/Reject quick-actions
  // to ride along whatever side-field comments the operator has
  // typed so far.
  onAnswerChange?: (answers: FormAnswer) => void;
  busy?: boolean;
  // - "auto" (default): 1Q inline, 2+Q multi-step wizard.
  // - "wizard": always multi-step, even for 1Q.
  // - "flat": legacy single-page rendering (used for compatibility).
  mode?: WizardMode;
  // When true, no Submit / Send button is rendered: the wizard is
  // navigation-only and an external control drives the final submit
  // (e.g. RunView's Approve / Reject buttons reading from
  // onAnswerChange).
  hideSubmit?: boolean;
}

// WizardForm renders a FormSpec. With multiple questions and the
// default "auto" mode it paginates one question per step, with a
// Claude-Code-style stepper (progress dots, Back/Next/Submit, atomic
// submit on the last step).
export function WizardForm({
  spec,
  onSubmit,
  onAnswerChange,
  busy = false,
  mode = "auto",
  hideSubmit = false,
}: WizardFormProps) {
  const initial = useMemo(() => initialAnswers(spec), [spec]);
  const [answers, setAnswers] = useState<FormAnswer>(initial);
  const [step, setStep] = useState(0);
  const [visited, setVisited] = useState<Set<number>>(() => new Set([0]));

  // Reset internal state when the spec identity changes (new pause).
  useEffect(() => {
    setAnswers(initial);
    setStep(0);
    setVisited(new Set([0]));
  }, [initial]);

  const total = spec.questions.length;
  const useWizard =
    mode === "wizard" || (mode === "auto" && total >= 2);
  const single = total === 1;
  const submitLabel = spec.submitLabel ?? "Send";

  const setOne = useCallback(
    (id: string, value: string | string[]) => {
      setAnswers((prev) => {
        const next = { ...prev, [id]: value };
        if (onAnswerChange) onAnswerChange(next);
        return next;
      });
    },
    [onAnswerChange],
  );

  const submit = () => {
    if (busy || !isFormValid(spec, answers)) return;
    onSubmit(answers);
  };

  // ── Flat / inline rendering (1Q in auto mode, or "flat" mode) ──
  if (!useWizard) {
    return (
      <FlatForm
        spec={spec}
        answers={answers}
        onAnswerChange={setOne}
        onSubmit={submit}
        busy={busy}
        single={single}
        submitLabel={submitLabel}
        hideSubmit={hideSubmit}
      />
    );
  }

  // ── Wizard (multi-step) ──
  // Guard against out-of-range step indices (e.g. if `total` changes
  // after a question is removed from the spec mid-flow). When no
  // current question exists we render nothing meaningful — return null
  // so the rest of this branch can treat `current` as defined.
  const current = spec.questions[step];
  if (!current) return null;
  const isLast = step === total - 1;
  const stepValid = isQuestionValid(current, answers[current.id]);
  const canSubmitAll = isFormValid(spec, answers);

  const goTo = (next: number) => {
    if (next < 0 || next >= total) return;
    setStep(next);
    setVisited((prev) => {
      if (prev.has(next)) return prev;
      const out = new Set(prev);
      out.add(next);
      return out;
    });
  };

  const onNext = () => {
    if (!stepValid || busy) return;
    if (isLast) submit();
    else goTo(step + 1);
  };

  const onBack = () => {
    if (step === 0 || busy) return;
    goTo(step - 1);
  };

  const onKeyDown = (e: React.KeyboardEvent<HTMLDivElement>) => {
    // Cmd/Ctrl-Enter submits on last step.
    if (isLast && (e.metaKey || e.ctrlKey) && e.key === "Enter") {
      e.preventDefault();
      submit();
      return;
    }
    // Plain Enter on non-textarea advances to next.
    if (
      e.key === "Enter" &&
      !e.shiftKey &&
      !(e.target instanceof HTMLTextAreaElement)
    ) {
      e.preventDefault();
      onNext();
    }
  };

  const tabItems: TabItem[] = spec.questions.map((_q, i) => {
    const reachable = visited.has(i) || i <= step;
    return {
      value: String(i),
      label: (
        <span
          aria-label={`Question ${i + 1} of ${total}`}
          className={`block w-2 h-2 rounded-full transition-colors ${
            i === step
              ? "bg-accent"
              : visited.has(i)
                ? "bg-accent/40"
                : "bg-border-default"
          }`}
        />
      ),
      disabled: !reachable,
    };
  });

  return (
    <div
      className="rounded-lg border border-border-subtle bg-surface-1 p-3 space-y-3"
      onKeyDown={onKeyDown}
    >
      <div className="flex items-center justify-between gap-3">
        <div className="text-[11px] font-medium text-fg-muted">
          Question {step + 1} of {total}
        </div>
        <Tabs
          value={String(step)}
          onValueChange={(v) => {
            const next = Number(v);
            if (visited.has(next)) goTo(next);
          }}
          items={tabItems}
          variant="pill"
          listClassName="!p-0 !gap-1"
        />
      </div>

      <div className="space-y-1">
        <div className="space-y-0.5">
          <label className="text-[12px] font-medium text-fg-default">
            {current.label}
            {isRequiredQuestion(current) && (
              <span className="ml-1 text-danger-fg" aria-hidden="true">
                *
              </span>
            )}
          </label>
          {current.description && (
            <p className="text-[11px] text-fg-muted">{current.description}</p>
          )}
        </div>
        <QuestionInput
          question={current}
          value={answers[current.id]}
          onChange={(v) => setOne(current.id, v)}
          disabled={busy}
        />
      </div>

      <div className="flex items-center justify-between pt-1">
        <Button
          variant="secondary"
          size="sm"
          disabled={step === 0 || busy}
          onClick={onBack}
        >
          Back
        </Button>
        <div className="flex items-center gap-2">
          {!isLast && !isRequiredQuestion(current) && (
            <Button
              variant="ghost"
              size="sm"
              disabled={busy}
              onClick={() => goTo(step + 1)}
            >
              Skip
            </Button>
          )}
          {renderForwardButton({
            isLast,
            hideSubmit,
            busy,
            canSubmitAll,
            stepValid,
            submitLabel,
            onSubmit: submit,
            onNext,
          })}
        </div>
      </div>
    </div>
  );
}

interface ForwardButtonProps {
  isLast: boolean;
  hideSubmit: boolean;
  busy: boolean;
  canSubmitAll: boolean;
  stepValid: boolean;
  submitLabel: string;
  onSubmit: () => void;
  onNext: () => void;
}

function renderForwardButton({
  isLast,
  hideSubmit,
  busy,
  canSubmitAll,
  stepValid,
  submitLabel,
  onSubmit,
  onNext,
}: ForwardButtonProps) {
  if (isLast && hideSubmit) return null;
  if (isLast) {
    return (
      <Button
        variant="primary"
        size="sm"
        loading={busy}
        disabled={!canSubmitAll || busy}
        onClick={onSubmit}
      >
        {busy ? "Submitting…" : submitLabel}
      </Button>
    );
  }
  return (
    <Button
      variant="primary"
      size="sm"
      disabled={!stepValid || busy}
      onClick={onNext}
    >
      Next →
    </Button>
  );
}

// ── Flat / inline form (no pagination) ──

interface FlatFormProps {
  spec: FormSpec;
  answers: FormAnswer;
  onAnswerChange: (id: string, value: string | string[]) => void;
  onSubmit: () => void;
  busy: boolean;
  single: boolean;
  submitLabel: string;
  hideSubmit: boolean;
}

function FlatForm({
  spec,
  answers,
  onAnswerChange,
  onSubmit,
  busy,
  single,
  submitLabel,
  hideSubmit,
}: FlatFormProps) {
  const valid = isFormValid(spec, answers);
  return (
    <div
      className={
        single
          ? "flex items-stretch gap-2"
          : "rounded-lg border border-border-subtle bg-surface-1 p-3 space-y-3"
      }
    >
      <div className={single ? "flex-1" : "space-y-3"}>
        {spec.questions.map((q, i) => (
          <div key={q.id} className={single ? "" : "space-y-1"}>
            {!single && (
              <div className="space-y-0.5">
                <label className="text-[12px] font-medium text-fg-default">
                  {q.label}
                  {isRequiredQuestion(q) && (
                    <span className="ml-1 text-danger-fg" aria-hidden="true">
                      *
                    </span>
                  )}
                </label>
                {q.description && (
                  <p className="text-[11px] text-fg-muted">{q.description}</p>
                )}
              </div>
            )}
            <QuestionInput
              question={q}
              value={answers[q.id]}
              onChange={(v) => onAnswerChange(q.id, v)}
              disabled={busy}
            />
            {single && i === 0 && q.description && (
              <p className="mt-1 text-[11px] text-fg-muted">{q.description}</p>
            )}
          </div>
        ))}
      </div>

      {!hideSubmit && (
        <div className={single ? "self-end" : "flex justify-end pt-1"}>
          <Button
            variant="primary"
            size="sm"
            loading={busy}
            disabled={!valid || busy}
            onClick={onSubmit}
          >
            {busy ? "Submitting…" : submitLabel}
          </Button>
        </div>
      )}
    </div>
  );
}

// ── Validation helpers ──

function initialAnswers(spec: FormSpec): FormAnswer {
  const out: FormAnswer = {};
  for (const q of spec.questions) {
    if (q.kind === "checkbox") {
      out[q.id] = q.defaultValues ? [...q.defaultValues] : [];
    } else {
      out[q.id] = "";
    }
  }
  return out;
}

export function isFormValid(spec: FormSpec, answers: FormAnswer): boolean {
  for (const q of spec.questions) {
    if (!isQuestionValid(q, answers[q.id])) return false;
  }
  return true;
}

export function isQuestionValid(
  q: FormQuestion,
  v: string | string[] | undefined,
): boolean {
  if (!isRequiredQuestion(q)) return true;
  if (q.kind === "checkbox") {
    if (!Array.isArray(v) || v.length === 0) return false;
    const meaningful = v.filter(
      (x) => x !== OTHER_SENTINEL && x.trim() !== "",
    );
    return meaningful.length > 0;
  }
  if (typeof v !== "string") return false;
  if (v.trim() === "" || v === OTHER_SENTINEL) return false;
  return true;
}

export function isRequiredQuestion(q: FormQuestion): boolean {
  if (q.required !== undefined) return q.required;
  // Defaults: radio + free_text required; checkbox + select optional.
  return q.kind === "radio" || q.kind === "free_text";
}

export default WizardForm;
