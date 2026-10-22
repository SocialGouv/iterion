import { useId, useState } from "react";

import { Checkbox, Input, Radio, Select, Textarea } from "@/components/ui";
import GateFileInput from "@/components/shared/GateFileInput";
import type {
  FormQuestion,
  QuestionOption,
} from "@/lib/whats-next/questionForm";
import { OTHER_SENTINEL } from "@/lib/whats-next/questionForm";

interface Props {
  question: FormQuestion;
  // Current value. String for radio/select/free_text; string[] for
  // checkbox. The renderer also tracks the "Other"-typed text
  // internally and emits the merged result through onChange.
  value: string | string[] | undefined;
  onChange: (next: string | string[]) => void;
  disabled?: boolean;
  /** id for the primary control of kinds that have one (select, free_text): the
   *  caller's visible <label htmlFor> points here (RGAA 11.1). Kinds rendered
   *  as grouped rows name each row instead. */
  inputId?: string;
}

export default function QuestionInput({
  question,
  value,
  onChange,
  disabled = false,
  inputId,
}: Props) {
  switch (question.kind) {
    case "radio":
      return (
        <RadioInput
          question={question}
          value={typeof value === "string" ? value : ""}
          onChange={onChange}
          disabled={disabled}
        />
      );
    case "checkbox":
      return (
        <CheckboxInput
          question={question}
          value={Array.isArray(value) ? value : []}
          onChange={onChange}
          disabled={disabled}
        />
      );
    case "select":
      return (
        <SelectInput
          question={question}
          value={typeof value === "string" ? value : ""}
          onChange={onChange}
          disabled={disabled}
          inputId={inputId}
        />
      );
    case "free_text":
      return (
        <FreeTextInput
          question={question}
          value={typeof value === "string" ? value : ""}
          onChange={onChange}
          disabled={disabled}
          inputId={inputId}
        />
      );
    case "file":
      return (
        <FileQuestionInput
          question={question}
          value={typeof value === "string" ? value : ""}
          onChange={onChange}
          disabled={disabled}
        />
      );
  }
}

// ─── File ──────────────────────────────────────────────────────────

function FileQuestionInput({
  question,
  value,
  onChange,
  disabled,
}: {
  question: Extract<FormQuestion, { kind: "file" }>;
  value: string;
  onChange: (next: string) => void;
  disabled: boolean;
}) {
  // The ANSWER is just the staged upload id (FormAnswer values are
  // strings, which is what lets file questions ride the existing
  // validation/submit plumbing untouched). Filename and size are
  // presentation-only, so they live here rather than being smuggled
  // into the answer string.
  const [meta, setMeta] = useState<{ filename: string; size: number } | null>(
    null,
  );

  return (
    <GateFileInput
      label={question.label}
      accept={question.accept}
      hint={question.hint}
      disabled={disabled}
      value={
        value
          ? { uploadId: value, filename: meta?.filename ?? "Attached file", size: meta?.size ?? 0 }
          : null
      }
      onChange={(next) => {
        setMeta(next ? { filename: next.filename, size: next.size } : null);
        onChange(next?.uploadId ?? "");
      }}
    />
  );
}

// ─── Radio ─────────────────────────────────────────────────────────

function RadioInput({
  question,
  value,
  onChange,
  disabled,
}: {
  question: Extract<FormQuestion, { kind: "radio" }>;
  value: string;
  onChange: (next: string) => void;
  disabled: boolean;
}) {
  const groupId = useId();
  const [otherText, setOtherText] = useState(() =>
    valueIsOther(value, question.options) ? value : "",
  );
  // Track "Other is the explicitly-selected row" as local state, NOT
  // derived from value-match. Otherwise typing a free-text answer
  // that happens to equal a canned option value (e.g. user types
  // "Ship a specific feature" into Other and that string is also an
  // option) immediately highlights the canned row and unchecks Other.
  const [otherSelected, setOtherSelected] = useState(() =>
    valueIsOther(value, question.options),
  );
  const selectedSentinel =
    value === ""
      ? ""
      : otherSelected
        ? OTHER_SENTINEL
        : matchesOption(value, question.options)
          ? value
          : OTHER_SENTINEL;

  return (
    <div className="space-y-1.5">
      {question.options.map((opt) => (
        <OptionRow
          key={opt.value}
          name={groupId}
          type="radio"
          option={opt}
          checked={selectedSentinel === opt.value}
          disabled={disabled}
          onSelect={() => {
            setOtherSelected(false);
            onChange(opt.value);
          }}
        />
      ))}
      {question.allow_other && (
        <OtherRow
          name={groupId}
          type="radio"
          checked={selectedSentinel === OTHER_SENTINEL}
          otherText={otherText}
          disabled={disabled}
          onSelect={() => {
            setOtherSelected(true);
            onChange(otherText || OTHER_SENTINEL);
          }}
          onOtherTextChange={(t) => {
            setOtherText(t);
            setOtherSelected(true);
            // Re-emit so the form-level submit gate sees the
            // up-to-date text. Empty text keeps OTHER_SENTINEL so
            // the row stays "selected" but the form submit stays
            // gated until the user actually types something.
            onChange(t || OTHER_SENTINEL);
          }}
        />
      )}
    </div>
  );
}

// ─── Checkbox ──────────────────────────────────────────────────────

function CheckboxInput({
  question,
  value,
  onChange,
  disabled,
}: {
  question: Extract<FormQuestion, { kind: "checkbox" }>;
  value: string[];
  onChange: (next: string[]) => void;
  disabled: boolean;
}) {
  const groupId = useId();
  const optValues = new Set(question.options.map((o) => o.value));
  // The Other text is owned by component state — separate from
  // `value` so unchecking Other doesn't lose what the user typed.
  // On mount, seed from any non-option entry that's not the
  // sentinel.
  const initialOther = value.find(
    (v) => !optValues.has(v) && v !== OTHER_SENTINEL,
  );
  const [otherText, setOtherText] = useState(initialOther ?? "");
  // Other is "selected" when value carries either typed text OR the
  // sentinel placeholder (the latter signals "Other clicked, no
  // text yet" — submit stays gated).
  const otherSelected = value.some((v) => !optValues.has(v));

  const toggleOption = (optVal: string) => {
    const next = value.includes(optVal)
      ? value.filter((v) => v !== optVal)
      : [...value, optVal];
    onChange(next);
  };

  return (
    <div className="space-y-1.5">
      {question.options.map((opt) => (
        <OptionRow
          key={opt.value}
          name={groupId}
          type="checkbox"
          option={opt}
          checked={value.includes(opt.value)}
          disabled={disabled}
          onSelect={() => toggleOption(opt.value)}
        />
      ))}
      {question.allow_other && (
        <OtherRow
          name={groupId}
          type="checkbox"
          checked={otherSelected}
          otherText={otherText}
          disabled={disabled}
          onSelect={() => {
            const onlyOptions = value.filter((v) => optValues.has(v));
            if (otherSelected) {
              onChange(onlyOptions);
            } else {
              onChange([...onlyOptions, otherText || OTHER_SENTINEL]);
            }
          }}
          onOtherTextChange={(t) => {
            setOtherText(t);
            const onlyOptions = value.filter((v) => optValues.has(v));
            // Only mutate the value array when Other is selected.
            if (otherSelected) {
              onChange([...onlyOptions, t || OTHER_SENTINEL]);
            }
          }}
        />
      )}
    </div>
  );
}

// ─── Select ────────────────────────────────────────────────────────

function SelectInput({
  question,
  value,
  onChange,
  disabled,
  inputId,
}: {
  question: Extract<FormQuestion, { kind: "select" }>;
  value: string;
  onChange: (next: string) => void;
  disabled: boolean;
  inputId?: string;
}) {
  return (
    <Select
      id={inputId}
      value={value}
      onChange={(e) => onChange(e.target.value)}
      disabled={disabled}
    >
      {question.placeholder && (
        <option value="" disabled>
          {question.placeholder}
        </option>
      )}
      {question.options.map((opt) => (
        <option key={opt.value} value={opt.value}>
          {opt.label}
        </option>
      ))}
    </Select>
  );
}

// ─── Free text ─────────────────────────────────────────────────────

function FreeTextInput({
  question,
  value,
  onChange,
  disabled,
  inputId,
}: {
  question: Extract<FormQuestion, { kind: "free_text" }>;
  value: string;
  onChange: (next: string) => void;
  disabled: boolean;
  inputId?: string;
}) {
  const rows = question.rows ?? 3;
  if (rows <= 1) {
    return (
      <Input
        id={inputId}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={question.placeholder}
        disabled={disabled}
      />
    );
  }
  return (
    <Textarea
      id={inputId}
      value={value}
      onChange={(e) => onChange(e.target.value)}
      placeholder={question.placeholder}
      rows={rows}
      disabled={disabled}
    />
  );
}

// ─── Shared option rows ────────────────────────────────────────────

function OptionRow({
  name,
  type,
  option,
  checked,
  disabled,
  onSelect,
}: {
  name: string;
  type: "radio" | "checkbox";
  option: QuestionOption;
  checked: boolean;
  disabled: boolean;
  onSelect: () => void;
}) {
  return (
    <label
      className={`flex items-start gap-2 text-body rounded px-2 py-1.5 cursor-pointer border ${
        checked
          ? "border-accent/40 bg-accent-soft"
          : // border-border-default, not -subtle: these rows sit on a
            // surface-1 card, and in dark theme border-subtle is the
            // SAME hex as surface-1 — the cards read as floating text.
            "border-border-default hover:bg-surface-2"
      } data-[disabled]:opacity-60 data-[disabled]:cursor-not-allowed`}
      data-disabled={disabled || undefined}
    >
      {type === "radio" ? (
        <Radio
          name={name}
          checked={checked}
          disabled={disabled}
          onChange={onSelect}
          className="mt-0.5"
        />
      ) : (
        <Checkbox
          name={name}
          checked={checked}
          disabled={disabled}
          onChange={onSelect}
          className="mt-0.5"
        />
      )}
      <div className="flex-1 min-w-0">
        <div className="text-fg-default">{option.label}</div>
        {option.description && (
          <div className="text-micro text-fg-muted">{option.description}</div>
        )}
      </div>
    </label>
  );
}

function OtherRow({
  name,
  type,
  checked,
  otherText,
  disabled,
  onSelect,
  onOtherTextChange,
}: {
  name: string;
  type: "radio" | "checkbox";
  checked: boolean;
  otherText: string;
  disabled: boolean;
  onSelect: () => void;
  onOtherTextChange: (next: string) => void;
}) {
  // The "Other" label points at the control it names (RGAA 11.1): without
  // the htmlFor, neither the choice control nor the free-text field has a
  // label.
  const otherId = useId();
  return (
    <div
      className={`flex items-start gap-2 text-body rounded px-2 py-1.5 border ${
        checked
          ? "border-accent/40 bg-accent-soft"
          : // Same dark-theme rationale as OptionRow above.
            "border-border-default"
      } data-[disabled]:opacity-60`}
      data-disabled={disabled || undefined}
    >
      {type === "radio" ? (
        <Radio
          name={name}
          id={otherId}
          checked={checked}
          disabled={disabled}
          onChange={onSelect}
          className="mt-0.5"
        />
      ) : (
        <Checkbox
          name={name}
          id={otherId}
          checked={checked}
          disabled={disabled}
          onChange={onSelect}
          className="mt-0.5"
        />
      )}
      <div className="flex-1 min-w-0 space-y-1">
        <label htmlFor={otherId} className="text-fg-default cursor-pointer">Other</label>
        <Input
          value={otherText}
          placeholder="Type your own answer…"
          aria-label="Other answer"
          disabled={disabled || !checked}
          onChange={(e) => onOtherTextChange(e.target.value)}
          onFocus={() => {
            if (!checked) onSelect();
          }}
        />
      </div>
    </div>
  );
}

// ─── Helpers ───────────────────────────────────────────────────────

function matchesOption(value: string, options: QuestionOption[]): boolean {
  return options.some((o) => o.value === value);
}

function valueIsOther(value: string, options: QuestionOption[]): boolean {
  return value !== "" && !matchesOption(value, options);
}
