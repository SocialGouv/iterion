// Pure classification helpers for the Launch form's var fields.
// Covered by varClassify.test.ts — no React here.

import type { VarField } from "@/api/types";

import { isVarRequired } from "@/lib/varValidation";

/** Runner-resolved infra placeholders. A default containing one of these
 *  (e.g. `${PROJECT_DIR}/report.md`) is resolved by the engine at run
 *  start — surfacing it as an editable input just invites operators to
 *  paste a literal path that breaks worktree/sandbox remapping. */
const AUTO_MANAGED_RE = /\$\{PROJECT(_SCRATCH)?_DIR\}/;

/** True when a default string references a runner-resolved placeholder. */
export function isAutoManagedDefault(def: string): boolean {
  return AUTO_MANAGED_RE.test(def);
}

/** True when the var's declared default references `${PROJECT_DIR}` /
 *  `${PROJECT_SCRATCH_DIR}`. Checks both the decoded string value and the
 *  raw source literal — some literal kinds only carry `raw`. */
export function isAutoManagedVar(field: VarField): boolean {
  const lit = field.default;
  if (!lit) return false;
  return (
    isAutoManagedDefault(lit.str_val ?? "") || isAutoManagedDefault(lit.raw ?? "")
  );
}

export type VarGroup = "primary" | "advanced" | "auto";

/** Bucket a var for progressive disclosure:
 *  - `auto`     — default references a runner-resolved placeholder;
 *                 read-only row under Advanced (override opt-in).
 *  - `primary`  — required (no default); always visible.
 *  - `advanced` — optional with a default; under Advanced. */
export function classifyVar(field: VarField): VarGroup {
  if (isAutoManagedVar(field)) return "auto";
  if (isVarRequired(field)) return "primary";
  return "advanced";
}
