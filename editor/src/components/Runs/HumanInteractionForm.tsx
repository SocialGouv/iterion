import { useMemo, useState } from "react";

import type { WireSchemaField } from "@/api/runs";

import { Button } from "@/components/ui";

import HumanInteractionField, { coerceField } from "./HumanInteractionField";

interface Props {
  fields: WireSchemaField[];
  questions: Record<string, unknown>;
  busy?: boolean;
  errorMessage?: string | null;
  onSubmit: (answers: Record<string, unknown>) => void;
}

export default function HumanInteractionForm({
  fields,
  questions,
  busy = false,
  errorMessage,
  onSubmit,
}: Props) {
  const initialDrafts = useMemo(
    () => Object.fromEntries(fields.map((f) => [f.name, defaultDraft(f)])),
    [fields],
  );
  const [drafts, setDrafts] = useState<Record<string, string>>(initialDrafts);
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});

  const setDraft = (name: string, next: string) => {
    setDrafts((prev) => ({ ...prev, [name]: next }));
    if (fieldErrors[name]) {
      setFieldErrors((prev) => {
        const { [name]: _, ...rest } = prev;
        return rest;
      });
    }
  };

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    const errs: Record<string, string> = {};
    const answers: Record<string, unknown> = {};
    for (const f of fields) {
      const raw = drafts[f.name] ?? "";
      const { value, error } = coerceField(f, raw);
      if (error) {
        errs[f.name] = error;
        continue;
      }
      if (value !== undefined) {
        answers[f.name] = value;
      }
    }
    if (Object.keys(errs).length > 0) {
      setFieldErrors(errs);
      return;
    }
    onSubmit(answers);
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
      <div className="flex gap-2">
        <Button type="submit" variant="primary" size="sm" disabled={busy}>
          {busy ? "Resuming…" : "Submit & Resume"}
        </Button>
      </div>
    </form>
  );
}

function defaultDraft(field: WireSchemaField): string {
  if (field.type === "bool") return "false";
  return "";
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
