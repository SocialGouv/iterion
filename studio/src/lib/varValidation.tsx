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

/** Small reused affordance — the "required" pill next to a field
 *  label. Lives here so both LaunchView and the board ticket form
 *  pick it up. */
export function RequiredPill() {
  return (
    <span className="text-[10px] text-warning-fg uppercase tracking-wide">required</span>
  );
}
