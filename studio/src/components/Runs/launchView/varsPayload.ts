// Pure payload builder for the Launch form's vars map.
// Covered by varsPayload.test.ts — no React here.

import type { VarField } from "@/api/types";

import { defaultStringFor } from "@/components/shared/VarFieldInput";

/** Build the `vars` payload for createRun: only vars whose current form
 *  value differs from their effective baseline are sent. The baseline is
 *  the declared default — or, for keys the active preset covers, the
 *  preset's value, because the server applies preset values first and
 *  only spec.Vars override them: a field the operator edited away from
 *  the preset (even back to the declared default) must be sent or the
 *  preset silently wins. Untouched baselines are OMITTED so the server
 *  applies its own default — including `${PROJECT_DIR}` / `${...}`
 *  expansion, which a client echo of the raw default string would break.
 *  Returns undefined when nothing was touched. */
export function buildVarsPayload(
  fields: VarField[],
  values: Record<string, string>,
  presetValues?: Record<string, string>,
): Record<string, string> | undefined {
  const out: Record<string, string> = {};
  for (const f of fields) {
    const cur = values[f.name];
    if (cur === undefined) continue;
    const baseline =
      presetValues && f.name in presetValues
        ? presetValues[f.name]
        : defaultStringFor(f);
    if (cur === baseline) continue;
    out[f.name] = cur;
  }
  return Object.keys(out).length > 0 ? out : undefined;
}
