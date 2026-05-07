import { useMemo, useState } from "react";

import type { WireSchemaField } from "@/api/runs";

import { Button } from "@/components/ui";

import HumanInteractionField, { coerceField } from "./HumanInteractionField";

interface Props {
  // The schema fields declared on the paused HumanNode. May be empty:
  // a node without `output:` schema gets a free-text fallback handled
  // by HumanInteractionPanel, not here.
  fields: WireSchemaField[];
  // Per-field question text from the runtime's interaction_questions
  // map. Indexed by field name. Falls back to the field name itself
  // when absent.
  questions: Record<string, unknown>;
  // Optional one-line node-level guidance from HumanNode.Instructions
  // (rendered above the form by the panel, not here).
  busy?: boolean;
  errorMessage?: string | null;
  onSubmit: (answers: Record<string, unknown>) => void;
}

/** Schema-driven form generator. Collects strings while editing,
 *  coerces them to the runtime types on submit, and posts a
 *  Record<string, unknown> matching the answers contract of
 *  POST /api/runs/{id}/resume. */
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
      // value === undefined means "user left it blank" — omit
      // from the answers map. The runtime accepts partial answers.
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
