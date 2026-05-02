import type { VarField } from "@/api/types";

/**
 * Heuristics for the run-launch panel: should a string `vars:` field be
 * rendered as a multi-row textarea (prompt-like) or a single-line input?
 *
 * Pure functions only — covered by `promptVarHeuristics.test.ts`.
 */

/** Var names treated as prompt-like even without a suffix match. */
const PROMPT_LIKE_EXACT = new Set(["prompt", "description", "instructions"]);

/** Detect names like `feature_prompt`, `bug_description`, etc. */
const PROMPT_SUFFIX_RE = /(_prompt|_description)$/i;

/**
 * Returns true when the given var should render as a prompt-style
 * textarea instead of a single-line input. The caller is responsible
 * for the `string` type check; non-string vars always return false.
 */
export function isPromptLikeVar(field: VarField): boolean {
  if (field.type !== "string") return false;

  const lower = field.name.toLowerCase();
  if (PROMPT_LIKE_EXACT.has(lower)) return true;
  if (PROMPT_SUFFIX_RE.test(field.name)) return true;

  // Heuristic (c) — string vars without a default tend to be feature
  // prompts the workflow expects the launcher to fill in.
  if (!hasMeaningfulDefault(field)) return true;

  return false;
}

/** A `default` is "meaningful" when it would resolve to a non-empty string. */
function hasMeaningfulDefault(field: VarField): boolean {
  const lit = field.default;
  if (!lit) return false;
  if (lit.str_val !== undefined && lit.str_val.length > 0) return true;
  if (lit.raw && lit.raw !== '""' && lit.raw !== "''" && lit.raw !== "") return true;
  return false;
}

/**
 * Pick a sensible row-count for the textarea. Names that strongly
 * signal a prompt body (suffix rule) get a taller surface; the
 * heuristic-(c) "no default" case stays moderate so a one-liner
 * still looks fine.
 */
export function suggestRows(field: VarField): number {
  if (PROMPT_SUFFIX_RE.test(field.name)) return 8;
  const lower = field.name.toLowerCase();
  if (PROMPT_LIKE_EXACT.has(lower)) return 8;
  return 6;
}
