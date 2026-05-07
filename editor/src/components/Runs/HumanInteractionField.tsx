import type { WireSchemaField } from "@/api/runs";

import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { Textarea } from "@/components/ui/Textarea";

interface Props {
  field: WireSchemaField;
  // The form holds every value as a string while it's being edited;
  // type coercion happens on submit. This mirrors VarFieldInput's
  // contract so the parent doesn't need a discriminated union of
  // per-field state.
  value: string;
  onChange: (next: string) => void;
}

/** Per-type renderer for a single answer field on a paused human node.
 *
 * The form layer collects strings; HumanInteractionForm.coerceAnswers
 * casts them to the right runtime type before posting. Enums always
 * render as a Select regardless of base type (boolean enums, string
 * enums, etc.). */
export default function HumanInteractionField({ field, value, onChange }: Props) {
  if (field.enum_values && field.enum_values.length > 0) {
    return (
      <Select
        value={value}
        onChange={(e) => onChange(e.target.value)}
        size="sm"
      >
        <option value="" disabled>
          Choose…
        </option>
        {field.enum_values.map((opt) => (
          <option key={opt} value={opt}>
            {opt}
          </option>
        ))}
      </Select>
    );
  }

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
          <span className="text-xs text-fg-muted">
            {value === "true" ? "true" : "false"}
          </span>
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
          placeholder='{"key": "value"}'
        />
      );
    case "string[]":
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
        <Textarea
          value={value}
          onChange={(e) => onChange(e.target.value)}
          rows={value.length > 80 ? 4 : 2}
          spellCheck={false}
          className="text-[12px]"
        />
      );
  }
}

/** Coerce a raw string draft to the runtime type declared by the
 *  schema field. Returns { value, error } so callers can display a
 *  per-field validation message without throwing. */
export function coerceField(
  field: WireSchemaField,
  raw: string,
): { value: unknown; error: string | null } {
  // Empty input policy: required-ness lives one layer up
  // (HumanInteractionForm enforces "must answer at least one of the
  // declared fields" rather than per-field required, since the
  // backend accepts a partial answers map). An empty string here
  // serialises as "" for string types and as undefined for the
  // others — the form filters undefined entries before posting.
  if (raw === "" && field.type !== "string") {
    return { value: undefined, error: null };
  }

  if (field.enum_values && field.enum_values.length > 0) {
    if (raw === "") return { value: undefined, error: null };
    if (!field.enum_values.includes(raw)) {
      return { value: null, error: `must be one of: ${field.enum_values.join(", ")}` };
    }
    return { value: raw, error: null };
  }

  switch (field.type) {
    case "bool":
      return { value: raw === "true", error: null };
    case "int": {
      const n = parseInt(raw, 10);
      if (Number.isNaN(n)) return { value: null, error: "must be an integer" };
      return { value: n, error: null };
    }
    case "float": {
      const n = parseFloat(raw);
      if (Number.isNaN(n)) return { value: null, error: "must be a number" };
      return { value: n, error: null };
    }
    case "json": {
      try {
        return { value: JSON.parse(raw), error: null };
      } catch {
        return { value: null, error: "must be valid JSON" };
      }
    }
    case "string[]":
      return {
        value: raw
          .split(",")
          .map((s) => s.trim())
          .filter(Boolean),
        error: null,
      };
    case "string":
    default:
      return { value: raw, error: null };
  }
}
