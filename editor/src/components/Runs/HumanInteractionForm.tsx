import { useState } from "react";

import type { WireSchemaField } from "@/api/runs";

import { Button } from "@/components/ui";

import HumanInteractionField, { coerceField } from "./HumanInteractionField";

interface Props {
  fields: WireSchemaField[];
  questions: Record<string, unknown>;
  drafts: Record<string, string>;
  onDraftChange: (name: string, next: string) => void;
  busy?: boolean;
  errorMessage?: string | null;
  // When provided, renders a Submit button at the bottom; the parent
  // is responsible for coercing `drafts` and posting. Omit when an
  // outer component (e.g. quick-action Approve/Reject buttons) drives
  // submission instead.
  onSubmit?: () => void;
}

export default function HumanInteractionForm({
  fields,
  questions,
  drafts,
  onDraftChange,
  busy = false,
  errorMessage,
  onSubmit,
}: Props) {
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});

  const setDraft = (name: string, next: string) => {
    onDraftChange(name, next);
    if (fieldErrors[name]) {
      setFieldErrors((prev) => {
        const { [name]: _, ...rest } = prev;
        return rest;
      });
    }
  };

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!onSubmit) return;
    const errs = validateDrafts(fields, drafts);
    if (Object.keys(errs).length > 0) {
      setFieldErrors(errs);
      return;
    }
    onSubmit();
  };

  return (
    <form onSubmit={handleSubmit} className="space-y-3">
      {fields.map((field) => {
        const prompt = stringifyQuestion(questions[field.name]);
        const err = fieldErrors[field.name];
        return (
          <label key={field.name} className="block space-y-1">
            <div className="flex items-baseline justify-between gap-2">
              <span className="text-[11px] font-medium text-fg-default">
                {field.name}
              </span>
              <span className="text-[10px] text-fg-subtle font-mono">
                {field.type}
                {field.enum_values && field.enum_values.length > 0 ? " (enum)" : ""}
              </span>
            </div>
            {prompt && (
              <div className="text-[10px] text-fg-subtle whitespace-pre-wrap">
                {prompt}
              </div>
            )}
            <HumanInteractionField
              field={field}
              value={drafts[field.name] ?? ""}
              onChange={(next) => setDraft(field.name, next)}
            />
            {err && (
              <div className="text-[10px] text-danger-fg" role="alert">
                {err}
              </div>
            )}
          </label>
        );
      })}
      {errorMessage && (
        <p className="text-danger-fg text-[11px]" role="alert">
          {errorMessage}
        </p>
      )}
      {onSubmit && (
        <div className="flex gap-2">
          <Button type="submit" variant="primary" size="sm" disabled={busy}>
            {busy ? "Resuming…" : "Submit & Resume"}
          </Button>
        </div>
      )}
    </form>
  );
}

export function defaultDraft(field: WireSchemaField): string {
  if (field.type === "bool") return "false";
  return "";
}

export function buildInitialDrafts(
  fields: WireSchemaField[],
): Record<string, string> {
  return Object.fromEntries(fields.map((f) => [f.name, defaultDraft(f)]));
}

// coerceDrafts walks every field and produces the typed answers map
// the runtime expects. Returns errors when any draft fails coercion;
// callers should refuse to submit until errors is empty.
export function coerceDrafts(
  fields: WireSchemaField[],
  drafts: Record<string, string>,
): { answers: Record<string, unknown>; errors: Record<string, string> } {
  const answers: Record<string, unknown> = {};
  const errors: Record<string, string> = {};
  for (const f of fields) {
    const raw = drafts[f.name] ?? "";
    const { value, error } = coerceField(f, raw);
    if (error) {
      errors[f.name] = error;
      continue;
    }
    if (value !== undefined) {
      answers[f.name] = value;
    }
  }
  return { answers, errors };
}

function validateDrafts(
  fields: WireSchemaField[],
  drafts: Record<string, string>,
): Record<string, string> {
  return coerceDrafts(fields, drafts).errors;
}

function stringifyQuestion(q: unknown): string {
  if (typeof q === "string") return q;
  if (q == null) return "";
  try {
    return JSON.stringify(q);
  } catch {
    return String(q);
  }
}
