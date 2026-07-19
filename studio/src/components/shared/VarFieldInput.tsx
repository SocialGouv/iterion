import type { VarField } from "@/api/types";

import { Checkbox } from "@/components/ui/Checkbox";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { Textarea } from "@/components/ui/Textarea";
import { isEnumVar, isPromptLikeVar, suggestRows } from "@/lib/promptVarHeuristics";

interface Props {
  field: VarField;
  value: string;
  onChange: (next: string) => void;
  required?: boolean;
  invalid?: boolean;
  /** DOM id forwarded to the underlying control so a `<label htmlFor>`
   *  resolves to it (label-click focus) and the launch form can
   *  scroll/focus the first missing required field. */
  id?: string;
  /** Force the prompt-style textarea for a string var regardless of the
   *  name heuristics — e.g. a var the bot's launch hints promote to a
   *  primary input. Non-string types ignore it. Undefined falls back to
   *  isPromptLikeVar. */
  promptLike?: boolean;
}

/** Per-type renderer for a single workflow var input. The form layer
 *  collects everything as strings — `POST /api/runs` accepts vars as a
 *  string→string map and the engine resolves them to the declared type. */
export default function VarFieldInput({
  field,
  value,
  onChange,
  required,
  invalid,
  id,
  promptLike,
}: Props) {
  const common = {
    id,
    "aria-required": required || undefined,
    "aria-invalid": invalid || undefined,
  };
  switch (field.type) {
    case "bool":
      return (
        <label className="inline-flex items-center gap-2">
          <Checkbox
            checked={value === "true"}
            onChange={(e) => onChange(e.target.checked ? "true" : "false")}
            {...common}
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
          {...common}
        />
      );
    case "json":
      return (
        <Textarea
          value={value}
          onChange={(e) => onChange(e.target.value)}
          rows={4}
          spellCheck={false}
          className="font-mono text-micro"
          {...common}
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
          {...common}
        />
      );
    case "string":
    default:
      // Enum-constrained string vars render a fixed-choice select (the
      // same ui/Select the launch Engine-options pickers use). The enum
      // wins over the prompt-like heuristics AND a forced `promptLike`
      // (launch-hint prominence) — a closed value list is never a prompt
      // body. A current value outside the list (stale preset / query
      // param) stays visible as a disabled-but-selected "(invalid: x)"
      // option instead of silently snapping to another value.
      if (isEnumVar(field)) {
        const options = field.enum ?? [];
        const stale = !options.includes(value);
        return (
          <Select
            value={value}
            onChange={(e) => onChange(e.currentTarget.value)}
            error={invalid}
            {...common}
          >
            {stale &&
              (value === "" ? (
                <option value="" disabled>
                  Select a value…
                </option>
              ) : (
                <option value={value} disabled>
                  (invalid: {value})
                </option>
              ))}
            {options.map((opt) => (
              <option key={opt} value={opt}>
                {opt}
              </option>
            ))}
          </Select>
        );
      }
      // Long-form prompt-like fields (suffix _prompt/_description, exact
      // match on prompt/description/instructions, or any string var
      // declared without a default) get a multi-row monospace textarea
      // so authors can paste full prompt bodies comfortably.
      if (promptLike ?? isPromptLikeVar(field)) {
        return (
          <Textarea
            value={value}
            onChange={(e) => onChange(e.target.value)}
            rows={suggestRows(field)}
            spellCheck={false}
            className="font-mono text-body"
            placeholder={`Enter ${field.name}…`}
            {...common}
          />
        );
      }
      return (
        <Input
          value={value}
          onChange={(e) => onChange(e.target.value)}
          size="sm"
          {...common}
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
