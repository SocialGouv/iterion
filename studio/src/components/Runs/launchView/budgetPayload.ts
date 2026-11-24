// Pure payload-building logic for the Launch form's budget-override
// fields. Kept separate from BudgetSection.tsx so it can be unit-tested
// without rendering. The form keeps every field as a raw input string;
// buildBudgetPayload converts the set to the wire shape of
// CreateRunRequest.budget (pkg/server launchBudgetSpec) — or undefined
// when nothing is set, so the request omits `budget` entirely and the
// run inherits the bot's `budget:` block.

import type { RunBudget } from "@/api/runs";

export interface BudgetFieldValues {
  maxCostUsd: string;
  maxTokens: string;
  maxDuration: string;
  maxIterations: string;
  maxParallelBranches: string;
}

export const emptyBudgetFieldValues: BudgetFieldValues = {
  maxCostUsd: "",
  maxTokens: "",
  maxDuration: "",
  maxIterations: "",
  maxParallelBranches: "",
};

// positiveNumber parses a form string into a positive finite number.
// Empty, non-numeric, zero, and negative inputs all mean "inherit"
// (the server treats zero as inherit, so we never send it).
function positiveNumber(s: string): number | undefined {
  const t = s.trim();
  if (!t) return undefined;
  const n = Number(t);
  return Number.isFinite(n) && n > 0 ? n : undefined;
}

// positiveInteger is positiveNumber for the dimensions the wire carries as
// integers (tokens, iterations, parallel branches). The floor happens
// BEFORE the positivity test, so a "0.5" cannot floor to the 0 this module
// promises never to send — the defect the codemode twin carried, found by
// the revi gate on #3354. Cost stays fractional: it is dollars, not a count.
function positiveInteger(s: string): number | undefined {
  const n = positiveNumber(s);
  if (n === undefined) return undefined;
  const floored = Math.floor(n);
  return floored > 0 ? floored : undefined;
}

// buildBudgetPayload maps the form fields to the request's budget
// object. Integer dimensions are floored; max_duration is passed
// through verbatim (the server validates the Go duration syntax and
// 400s on a bad value — no client-side pre-parse to drift from it).
// Returns undefined when every field is empty/inherit.
export function buildBudgetPayload(
  fields: BudgetFieldValues,
): RunBudget | undefined {
  const out: RunBudget = {};
  const cost = positiveNumber(fields.maxCostUsd);
  if (cost !== undefined) out.max_cost_usd = cost;
  const tokens = positiveInteger(fields.maxTokens);
  if (tokens !== undefined) out.max_tokens = tokens;
  const duration = fields.maxDuration.trim();
  if (duration) out.max_duration = duration;
  const iterations = positiveInteger(fields.maxIterations);
  if (iterations !== undefined) out.max_iterations = iterations;
  const parallel = positiveInteger(fields.maxParallelBranches);
  if (parallel !== undefined) out.max_parallel_branches = parallel;
  return Object.keys(out).length > 0 ? out : undefined;
}
