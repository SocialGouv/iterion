import type { WireSchemaField } from "@/api/runs";

import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { Textarea } from "@/components/ui/Textarea";

interface Props {
  field: WireSchemaField;
  value: string;
  onChange: (next: string) => void;
}

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

export function coerceField(
  field: WireSchemaField,
  raw: string,
): { value: unknown; error: string | null } {
  // Backend accepts partial answers maps; blank non-string fields
  // serialise as undefined and the form drops them before posting.
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
