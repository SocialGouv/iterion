// Extracted from LaunchView.tsx to keep that file focused.
// VarFieldsSection renders one group of var inputs — one row per field,
// with two layouts (prompt-like vars get a vertical label/textarea,
// scalar vars get a 160px label + control grid). LaunchView renders it
// twice: required vars in the always-visible "Inputs" block, optional
// vars with defaults inside the Advanced disclosure. State (values,
// submit) is owned by LaunchView.

import type { ReactNode } from "react";

import type { VarField } from "@/api/types";

import VarFieldInput from "@/components/shared/VarFieldInput";
import { isEnumVar, isPromptLikeVar } from "@/lib/promptVarHeuristics";
import { isVarRequired, RequiredPill } from "@/lib/varValidation";

export interface VarFieldsSectionProps {
  fields: VarField[];
  values: Record<string, string>;
  submitting: boolean;
  onValueChange: (name: string, value: string) => void;
  onSubmit: () => void;
  /** Section heading; hidden when empty-string. */
  title?: string;
  /** Rendered instead of the form when `fields` is empty; null/undefined
   *  hides the section entirely. */
  emptyFallback?: ReactNode;
  /** String vars listed here render with prompt-style prominence
   *  (vertical label + textarea) even when the name heuristics wouldn't —
   *  the launch form passes the hint-forced primary names. */
  prominentNames?: ReadonlySet<string>;
}

export default function VarFieldsSection({
  fields,
  values,
  submitting,
  onValueChange,
  onSubmit,
  title = "Inputs",
  emptyFallback,
  prominentNames,
}: VarFieldsSectionProps) {
  if (fields.length === 0) return <>{emptyFallback ?? null}</>;
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        if (!submitting) onSubmit();
      }}
    >
      {title && <h2 className="text-xs font-medium text-fg-muted mb-2">{title}</h2>}
      <div className="space-y-4">
        {fields.map((f) => {
          // Enum vars never take the prompt-style layout — not via the
          // name/default heuristics (isPromptLikeVar already excludes
          // them) and not via launch-hint prominence: a closed choice
          // list stays a compact select row.
          const promptLike =
            !isEnumVar(f) &&
            (isPromptLikeVar(f) ||
              (f.type === "string" && !!prominentNames?.has(f.name)));
          const required = isVarRequired(f);
          const value = values[f.name] ?? "";
          const invalid = required && value.trim().length === 0;
          if (promptLike) {
            return (
              <div key={f.name} className="flex flex-col gap-1.5">
                <label htmlFor={`var-${f.name}`} className="flex items-baseline gap-2">
                  <span className="text-xs font-medium font-mono text-fg-default">{f.name}</span>
                  <span className="text-caption text-fg-subtle">{f.type}</span>
                  {required && <RequiredPill />}
                </label>
                <VarFieldInput
                  field={f}
                  id={`var-${f.name}`}
                  value={value}
                  onChange={(v) => onValueChange(f.name, v)}
                  required={required}
                  invalid={invalid}
                  promptLike={promptLike}
                />
              </div>
            );
          }
          return (
            <div key={f.name} className="grid grid-cols-[160px_1fr] gap-3 items-start">
              <label htmlFor={`var-${f.name}`} className="pt-1">
                <div className="flex items-baseline gap-2">
                  <span className="text-xs font-medium font-mono">{f.name}</span>
                  {required && <RequiredPill />}
                </div>
                <div className="text-caption text-fg-subtle">{f.type}</div>
              </label>
              <VarFieldInput
                field={f}
                id={`var-${f.name}`}
                value={value}
                onChange={(v) => onValueChange(f.name, v)}
                required={required}
                invalid={invalid}
              />
            </div>
          );
        })}
      </div>
    </form>
  );
}
