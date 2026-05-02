import type { VarField } from "@/api/types";

import { Input } from "@/components/ui/Input";
import { Textarea } from "@/components/ui/Textarea";
import { isPromptLikeVar, suggestRows } from "@/lib/promptVarHeuristics";

interface Props {
  field: VarField;
  value: string;
  onChange: (next: string) => void;
}

/** Per-type renderer for a single workflow var input. The form layer
 *  collects everything as strings — `POST /api/runs` accepts vars as a
 *  string→string map and the engine resolves them to the declared type. */
export default function VarFieldInput({ field, value, onChange }: Props) {
  switch (field.type) {
    case "bool":
      return (
        <label className="inline-flex items-center gap-2">
          <input
            type="checkbox"
            checked={value === "true"}
            onChange={(e) => onChange(e.target.checked ? "true" : "false")}
            className="accent-accent"
          />
          <span className="text-xs text-fg-muted">{value === "true" ? "true" : "false"}</span>
        </label>
      );
    case "int":
    case "float":
      return (
        <Input
          type="number"
          step={field.type === "float" ? "any" : "1"}
          value={value}
          onChange={(e) => onChange(e.target.value)}
          size="sm"
        />
      );
    case "json":
      return (
        <Textarea
          value={value}
          onChange={(e) => onChange(e.target.value)}
          rows={4}
          spellCheck={false}
          className="font-mono text-[11px]"
        />
      );
    case "string[]":
      // Simple comma-separated entry for v1. The backend accepts
      // string vars and the engine parses commas itself.
      return (
        <Input
          value={value}
          onChange={(e) => onChange(e.target.value)}
          placeholder="comma,separated,values"
          size="sm"
        />
      );
    case "string":
    default:
      // Long-form prompt-like fields (suffix _prompt/_description, exact
      // match on prompt/description/instructions, or any string var
      // declared without a default) get a multi-row monospace textarea
      // so authors can paste full prompt bodies comfortably.
      if (isPromptLikeVar(field)) {
        return (
          <Textarea
            value={value}
            onChange={(e) => onChange(e.target.value)}
            rows={suggestRows(field)}
            spellCheck={false}
            className="font-mono text-[12px]"
            placeholder={`Enter ${field.name}…`}
          />
        );
      }
      return (
        <Input
          value={value}
          onChange={(e) => onChange(e.target.value)}
          size="sm"
        />
      );
  }
}

/** Default-string for a var: render the literal default if present, else "".
 *
 * Dispatch is keyed off `lit.kind` (the source of truth) rather than the
 * presence of value-fields: empty-string defaults like `scope_notes: string = ""`
 * are encoded by the Go side without `str_val` (omitempty). Falling back on
 * `raw` for those would yield the literal source representation `""` (two
 * double-quote characters), pre-filling the form with garbage that then gets
 * sent verbatim to the backend.
 */
export function defaultStringFor(field: VarField): string {
  const lit = field.default;
  if (!lit) return field.type === "bool" ? "false" : "";
  switch (lit.kind) {
    case "string":
      return lit.str_val ?? "";
    case "int":
      return lit.int_val !== undefined ? String(lit.int_val) : "";
    case "float":
      return lit.float_val !== undefined ? String(lit.float_val) : "";
    case "bool":
      return lit.bool_val !== undefined ? String(lit.bool_val) : "false";
    default:
      return lit.raw ?? "";
  }
}
