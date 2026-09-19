// Pure helpers for the Bot-vars console (ITERION_* platform var overrides).
import type { BotVarsPatch } from "@/api/adminSettings";

export interface VarRow {
  // A stable client id so React keys survive rename/reorder.
  id: string;
  key: string;
  value: string;
}

let seq = 0;
export function newRow(key = "", value = ""): VarRow {
  seq += 1;
  return { id: `row-${seq}`, key, value };
}

// rowsFromVars builds the editable rows from the stored map (sorted for a
// stable display order).
export function rowsFromVars(vars: Record<string, string> | undefined): VarRow[] {
  const entries = Object.entries(vars ?? {}).sort(([a], [b]) => a.localeCompare(b));
  return entries.map(([k, v]) => newRow(k, v));
}

// buildBotVarsPatch diffs the edited rows against the stored map into the merge
// patch the server expects: a key whose value changed (or is new) → its value;
// a stored key no longer present (or blanked) → null (remove). A row with an
// empty key is ignored. Returns { patch, error } where error names a duplicate
// key (which would otherwise silently collapse two rows).
export function buildBotVarsPatch(
  rows: VarRow[],
  stored: Record<string, string> | undefined,
): { patch: BotVarsPatch; error?: string } {
  const storedMap = stored ?? {};
  const patch: BotVarsPatch = {};
  const seen = new Set<string>();
  const present = new Set<string>();

  for (const r of rows) {
    const key = r.key.trim();
    if (key === "") continue;
    if (seen.has(key)) {
      return { patch: {}, error: `duplicate key "${key}"` };
    }
    seen.add(key);
    present.add(key);
    const value = r.value;
    if (storedMap[key] !== value) patch[key] = value;
  }
  // Keys that were stored but no longer present → clear (null).
  for (const key of Object.keys(storedMap)) {
    if (!present.has(key)) patch[key] = null;
  }
  return { patch };
}
