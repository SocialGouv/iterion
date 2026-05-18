import type { WireSchemaField } from "@/api/runs";
import type {
  FormAnswer,
  FormQuestion,
  FormSpec,
  QuestionOption,
} from "@/lib/pilote/questionForm";

// formSpecFromSchema converts a workflow's human-node output_schema
// into a FormSpec the shared WizardForm can render. The questions map
// (resolved inbound edge data) is surfaced inline as each question's
// description so the operator can see the context to review while
// answering.
//
// Type mapping:
//   - enum_values (any base type) → radio (≤4 options) or select (5+)
//   - bool                        → radio [Yes / No]
//   - int / float                 → free_text rows=1 (numeric)
//   - json                        → free_text rows=4
//   - string[]                    → free_text rows=1 (comma-separated)
//   - string (default)            → free_text rows=2-3
//
// Defaults to required:true for every field because schema-driven
// human nodes are typically asking the operator for a decision; the
// caller can post-process to relax this if needed.
export function formSpecFromSchema(
  fields: WireSchemaField[],
  questions: Record<string, unknown>,
  opts: { submitLabel?: string } = {},
): FormSpec {
  return {
    questions: fields.map((f) => buildQuestion(f, questions[f.name])),
    submitLabel: opts.submitLabel,
  };
}

function buildQuestion(
  field: WireSchemaField,
  context: unknown,
): FormQuestion {
  const description = stringifyContext(context);
  const base = {
    id: field.name,
    label: field.name,
    description,
    required: true as const,
  };

  if (field.enum_values && field.enum_values.length > 0) {
    const options: QuestionOption[] = field.enum_values.map((v) => ({
      value: v,
      label: v,
    }));
    if (options.length <= 4) {
      return { ...base, kind: "radio", options };
    }
    return { ...base, kind: "select", options, placeholder: "Choose…" };
  }

  switch (field.type) {
    case "bool":
      return {
        ...base,
        kind: "radio",
        options: [
          { value: "true", label: "Yes" },
          { value: "false", label: "No" },
        ],
      };
    case "int":
    case "float":
      return {
        ...base,
        kind: "free_text",
        rows: 1,
        placeholder: field.type === "float" ? "123.45" : "123",
      };
    case "json":
      return {
        ...base,
        kind: "free_text",
        rows: 4,
        placeholder: '{"key": "value"}',
      };
    case "string[]":
      return {
        ...base,
        kind: "free_text",
        rows: 1,
        placeholder: "comma,separated,values",
      };
    case "string":
    default:
      return {
        ...base,
        kind: "free_text",
        rows: 3,
      };
  }
}

// coerceFormAnswerToSchema takes a wizard answer map (string |
// string[]) and converts each entry to the typed value the runtime
// expects. Returns a per-field errors map; empty when every field
// parsed cleanly.
export function coerceFormAnswerToSchema(
  fields: WireSchemaField[],
  answer: FormAnswer,
): { answers: Record<string, unknown>; errors: Record<string, string> } {
  const answers: Record<string, unknown> = {};
  const errors: Record<string, string> = {};
  for (const f of fields) {
    const v = answer[f.name];
    const raw = typeof v === "string" ? v : Array.isArray(v) ? v.join(",") : "";
    const { value, error } = coerceOne(f, raw);
    if (error) {
      errors[f.name] = error;
      continue;
    }
    if (value !== undefined) {
      answers[f.name] = value;
    }
  }
  return { answers, errors };
}

function coerceOne(
  field: WireSchemaField,
  raw: string,
): { value: unknown; error: string | null } {
  if (raw === "" && field.type !== "string") {
    return { value: undefined, error: null };
  }

  if (field.enum_values && field.enum_values.length > 0) {
    if (raw === "") return { value: undefined, error: null };
    if (!field.enum_values.includes(raw)) {
      return {
        value: null,
        error: `must be one of: ${field.enum_values.join(", ")}`,
      };
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

function stringifyContext(v: unknown): string | undefined {
  if (typeof v === "string") return v.length > 0 ? v : undefined;
  if (v == null) return undefined;
  try {
    return JSON.stringify(v, null, 2);
  } catch {
    return String(v);
  }
}
