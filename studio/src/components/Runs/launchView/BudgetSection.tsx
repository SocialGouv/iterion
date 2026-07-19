// Extracted-style sibling of WorktreeFinalizationSection: BudgetSection
// renders the collapsible "Budget overrides" block — per-run caps on
// cost / tokens / duration / iterations / parallel branches, the HTTP
// twin of the CLI --max-* flags. Presentational: LaunchView owns the
// field state and folds it into createRun() via buildBudgetPayload.

import { Input } from "@/components/ui/Input";

import type { BudgetFieldValues } from "./budgetPayload";

export interface BudgetSectionProps {
  show: boolean;
  values: BudgetFieldValues;
  onToggle: () => void;
  onChange: (patch: Partial<BudgetFieldValues>) => void;
}

interface RowSpec {
  key: keyof BudgetFieldValues;
  id: string;
  label: string;
  sub: string;
  type: "number" | "text";
  placeholder: string;
  step?: string;
  helper?: string;
}

const rows: RowSpec[] = [
  {
    key: "maxCostUsd",
    id: "launch-budget-max-cost",
    label: "max_cost_usd",
    sub: "LLM spend cap",
    type: "number",
    step: "0.01",
    placeholder: "inherits from the bot",
  },
  {
    key: "maxTokens",
    id: "launch-budget-max-tokens",
    label: "max_tokens",
    sub: "total token cap",
    type: "number",
    step: "1",
    placeholder: "inherits from the bot",
  },
  {
    key: "maxDuration",
    id: "launch-budget-max-duration",
    label: "max_duration",
    sub: "wall-clock cap",
    type: "text",
    placeholder: "2h",
    helper: "Go duration — overrides the bot's budget for this run",
  },
  {
    key: "maxIterations",
    id: "launch-budget-max-iterations",
    label: "max_iterations",
    sub: "loop-iteration cap",
    type: "number",
    step: "1",
    placeholder: "inherits from the bot",
  },
  {
    key: "maxParallelBranches",
    id: "launch-budget-max-parallel",
    label: "max_parallel_branches",
    sub: "branch concurrency",
    type: "number",
    step: "1",
    placeholder: "inherits from the bot",
  },
];

export default function BudgetSection({
  show,
  values,
  onToggle,
  onChange,
}: BudgetSectionProps) {
  return (
    <div className="mt-6 border-t border-border-default pt-4">
      <button
        type="button"
        className="text-xs text-fg-muted hover:text-fg-default flex items-center gap-1"
        onClick={onToggle}
        title="Cap this run's cost / tokens / duration without editing the bot."
      >
        <span>{show ? "▾" : "▸"}</span>
        <span>Budget overrides (cost / tokens / duration)</span>
      </button>
      {show && (
        <div className="mt-3 space-y-3 pl-4 border-l border-border-default">
          <div className="text-caption text-fg-subtle">
            Non-empty fields override the bot&apos;s <code>budget:</code> block
            for this run only — same precedence as the CLI{" "}
            <code>--max-*</code> flags. Empty = inherits from the bot.
          </div>
          {rows.map((row) => (
            <div
              key={row.key}
              className="grid grid-cols-[160px_1fr] gap-3 items-start"
            >
              <label htmlFor={row.id} className="pt-1">
                <div className="text-xs font-medium font-mono">{row.label}</div>
                <div className="text-caption text-fg-subtle">{row.sub}</div>
              </label>
              <div>
                <Input
                  id={row.id}
                  size="sm"
                  type={row.type}
                  min={row.type === "number" ? 0 : undefined}
                  step={row.step}
                  className="font-mono"
                  placeholder={row.placeholder}
                  value={values[row.key]}
                  onChange={(e) => onChange({ [row.key]: e.target.value })}
                />
                {row.helper && (
                  <div className="mt-1 text-caption text-fg-subtle">
                    {row.helper}
                  </div>
                )}
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
