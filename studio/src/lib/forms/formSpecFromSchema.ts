import type { WireSchemaField } from "@/api/runs";
import type {
  FormAnswer,
  FormQuestion,
  FormSpec,
  QuestionOption,
} from "@/lib/whats-next/questionForm";

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
//   - json with `_ids`-style name
//     + a sibling array of {id,…} → checkbox (multi-select over items)
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
    questions: fields.map((f) => buildQuestion(f, questions[f.name], questions)),
    submitLabel: opts.submitLabel,
  };
}

function buildQuestion(
  field: WireSchemaField,
  context: unknown,
  questions: Record<string, unknown>,
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
    case "json": {
      // Detect "multi-select over a sibling collection" — e.g. the
      // whats-next `selected_issue_ids: json` field which the operator
      // is meant to populate by checking boxes against the upstream
      // `created_issues` array. Without this branch the wizard would
      // ask the operator to type raw JSON, which is hostile UX (and
      // exactly the parity gap reported between /whats-next's
      // dedicated checkbox renderer and the generic run-view form).
      const items = isLikelySelectionField(field.name)
        ? findSelectableItems(questions, field.name)
        : null;
      if (items) {
        return {
          ...base,
          // Multi-select can be empty (e.g. "no, skip dispatch this
          // round"), so don't force a tick.
          required: false,
          kind: "checkbox",
          options: items,
        };
      }
      return {
        ...base,
        kind: "free_text",
        rows: 4,
        placeholder: '{"key": "value"}',
      };
    }
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

// isLikelySelectionField recognises the naming conventions a bot
// uses when it expects a multi-select over a sibling collection.
// The match is intentionally narrow — arbitrary `json` fields stay
// on the free-text path so non-selection payloads (config blobs,
// nested schemas) aren't accidentally shoehorned into a checkbox UI.
function isLikelySelectionField(name: string): boolean {
  const n = name.toLowerCase();
  return n.endsWith("_ids") || n.endsWith("_keys") || n.startsWith("selected_");
}

// findSelectableItems scans the questions context for a sibling
// array of objects that look like selectable items (each carries an
// `id` string). Returns option entries built from the first such
// array, or null when no usable collection is in scope. The item's
// `title` (or `name`) becomes the option label; `id` is always the
// option value because that's what the downstream agent expects.
function findSelectableItems(
  questions: Record<string, unknown>,
  selfName: string,
): QuestionOption[] | null {
  for (const [k, v] of Object.entries(questions)) {
    if (k === selfName) continue;
    if (!Array.isArray(v) || v.length === 0) continue;
    if (!v.every(looksLikeSelectable)) continue;
    return (v as Array<Record<string, unknown>>).map((it) => {
      const id = String(it.id);
      const titleSrc =
        typeof it.title === "string"
          ? it.title
          : typeof it.name === "string"
            ? it.name
            : "";
      // Optional secondary lines that pack useful context into the
      // option without making the label noisy.
      const meta: string[] = [];
      if (typeof it.assignee === "string" && it.assignee.length > 0) {
        meta.push(it.assignee);
      }
      if (typeof it.horizon === "string" && it.horizon.length > 0) {
        meta.push(it.horizon);
      }
      const shortId = id.replace(/^native:/, "").slice(0, 8);
      const label = titleSrc ? `${titleSrc} · ${shortId}` : id;
      return {
        value: id,
        label,
        description: meta.length > 0 ? meta.join(" · ") : undefined,
      };
    });
  }
  return null;
}

function looksLikeSelectable(v: unknown): boolean {
  return (
    typeof v === "object" &&
    v !== null &&
    !Array.isArray(v) &&
    typeof (v as Record<string, unknown>).id === "string" &&
    ((v as Record<string, unknown>).id as string).length > 0
  );
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
    // Checkbox → json mapping: a multi-select rendered over a sibling
    // collection (see buildQuestion's checkbox branch for json fields)
    // submits the picked ids as a string[]. The downstream agent
    // expects the JSON array directly — collapsing it through
    // `join(",")` + `JSON.parse` would either crash or yield a comma-
    // joined string masquerading as JSON.
    if (Array.isArray(v) && f.type === "json") {
      answers[f.name] = v;
      continue;
    }
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
