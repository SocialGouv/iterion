// Auto-managed vars — vars whose default references a runner-resolved
// placeholder (`${PROJECT_DIR}` / `${PROJECT_SCRATCH_DIR}`). They render
// as read-only rows under the Advanced disclosure: the runner expands
// the placeholder at start, and echoing the raw string back as an
// operator value would break worktree/sandbox path remapping. A per-row
// "Override" affordance unlocks a text input for power users; untouched
// rows are omitted from the launch payload (see varsPayload.ts).

import { useState } from "react";

import type { VarField } from "@/api/types";

import { Badge } from "@/components/ui/Badge";
import VarFieldInput, { defaultStringFor } from "@/components/shared/VarFieldInput";

export interface AutoManagedVarsSectionProps {
  fields: VarField[];
  values: Record<string, string>;
  submitting: boolean;
  onValueChange: (name: string, value: string) => void;
}

export default function AutoManagedVarsSection({
  fields,
  values,
  submitting,
  onValueChange,
}: AutoManagedVarsSectionProps) {
  const [overridden, setOverridden] = useState<Record<string, boolean>>({});
  if (fields.length === 0) return null;
  return (
    <section className="mt-4">
      <h3 className="text-xs font-medium text-fg-muted mb-1">Auto-managed inputs</h3>
      <p className="text-caption text-fg-subtle mb-2">
        Resolved by the runner at start — leave them alone unless you know
        the exact path you need.
      </p>
      <div className="space-y-2">
        {fields.map((f) => {
          const isOverridden = !!overridden[f.name];
          const def = defaultStringFor(f);
          return (
            <div key={f.name} className="grid grid-cols-[160px_1fr] gap-3 items-start">
              <label htmlFor={`var-${f.name}`} className="pt-1">
                <div className="flex items-baseline gap-2">
                  <span className="text-xs font-medium font-mono">{f.name}</span>
                  <Badge variant="info" size="sm">
                    auto
                  </Badge>
                </div>
                <div className="text-caption text-fg-subtle">{f.type}</div>
              </label>
              {isOverridden ? (
                <div>
                  <VarFieldInput
                    field={f}
                    id={`var-${f.name}`}
                    value={values[f.name] ?? ""}
                    onChange={(v) => onValueChange(f.name, v)}
                  />
                  <button
                    type="button"
                    className="mt-1 text-caption text-fg-subtle hover:text-fg-default underline"
                    disabled={submitting}
                    onClick={() => {
                      onValueChange(f.name, def);
                      setOverridden((prev) => ({ ...prev, [f.name]: false }));
                    }}
                  >
                    Reset to auto
                  </button>
                </div>
              ) : (
                <div className="flex flex-wrap items-center gap-2 pt-1">
                  <code
                    className="text-caption text-fg-muted font-mono truncate max-w-xs"
                    title={def}
                  >
                    {def}
                  </code>
                  <span className="text-caption text-fg-subtle">
                    Resolved by the runner at start
                  </span>
                  <button
                    type="button"
                    className="text-caption text-accent-text hover:underline"
                    disabled={submitting}
                    onClick={() =>
                      setOverridden((prev) => ({ ...prev, [f.name]: true }))
                    }
                  >
                    Override
                  </button>
                </div>
              )}
            </div>
          );
        })}
      </div>
    </section>
  );
}
