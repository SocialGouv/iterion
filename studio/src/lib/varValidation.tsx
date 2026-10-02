import type { VarField } from "@/api/types";

/** A var is required when the workflow declares no default. Bool
 *  fields always have an effective default ("false"), so they're
 *  never missing. */
export function isVarRequired(field: VarField): boolean {
  if (field.type === "bool") return false;
  return !field.default;
}

/** isVarMissing returns true when a required field is empty after
 *  trimming. Mirrors the LaunchView form's submit guard. */
export function isVarMissing(field: VarField, value: string): boolean {
  if (!isVarRequired(field)) return false;
  return value.trim().length === 0;
}

/** A var's `[matching: ...]` pattern is deliberately NOT checked here.
 *  The engine refuses an off-pattern value at launch, naming the var, the
 *  value and the pattern; the browser cannot reproduce that verdict.
 *
 *  Two reasons, both measured. The engine judges the value AFTER `${...}`
 *  expansion, which the form cannot do. And JavaScript's RegExp is not
 *  RE2: `.` excludes `\r` and U+2028/9 where RE2 excludes only `\n`, and
 *  `\S` is Unicode-aware where RE2's is ASCII — so `^\S+$` rejects a
 *  non-breaking space, a BOM or an em space in the browser and accepts
 *  them in the engine. A form built on that would disable Launch on a
 *  value the run would have served, and widening it by listing the
 *  divergent escapes is a guard that enumerates spellings, which does not
 *  converge. Checking it faithfully means asking the server.
 *
 *  Small reused affordance — the "required" pill next to a field
 *  label. Lives here so both LaunchView and the board ticket form
 *  pick it up. */
export function RequiredPill() {
  return (
    <span className="text-caption text-warning-fg uppercase tracking-wide">required</span>
  );
}
