import type { VarField } from "@/api/types";

import { Input } from "@/components/ui/Input";
import { Textarea } from "@/components/ui/Textarea";

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
      return (
        <Input
          value={value}
          onChange={(e) => onChange(e.target.value)}
          size="sm"
        />
      );
  }
}

/** Default-string for a var: render the literal default if present, else "". */
export function defaultStringFor(field: VarField): string {
  const lit = field.default;
  if (!lit) return field.type === "bool" ? "false" : "";
  if (lit.str_val !== undefined) return lit.str_val;
  if (lit.int_val !== undefined) return String(lit.int_val);
  if (lit.float_val !== undefined) return String(lit.float_val);
  if (lit.bool_val !== undefined) return String(lit.bool_val);
  return lit.raw ?? "";
}
