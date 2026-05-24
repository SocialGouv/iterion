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
  opts: { submitLabel?: string; mode?: FormSpec["mode"] } = {},
): FormSpec {
  return {
    questions: fields.map((f) => buildQuestion(f, questions[f.name], questions)),
    submitLabel: opts.submitLabel,
    mode: opts.mode,
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
          // Pre-tick every option so the form opens in
          // "create / dispatch everything" state. The operator
          // unticks items they want to drop instead of having to
          // hand-pick the ones they want — matches the way the
          // ask_which_to_process / human_review form is most often
          // used (the bot's proposal is mostly good, the operator
          // adjusts at the margin).
          defaultValues: items.map((o) => o.value),
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

// findSelectableItems scans the questions context for a collection
// of selectable items. Returns option entries built from the first
// usable array discovered, or null when nothing is in scope.
//
// Search order:
//   1. Direct sibling arrays at the questions root (e.g. the
//      `created_issues` array next to `selected_issue_ids` on the
//      whats-next ask_which_to_process node).
//   2. Arrays nested ONE LEVEL inside sibling objects, including a
//      singular "next_action"-shaped object treated as an array of
//      one. This covers the whats-next human_review case where the
//      review_input is `{roadmap: {long_term: [...], short_term: [...],
//      next_action: {...}, rationale: "..."}}` and selected_titles
//      needs to expand into a checkbox column across every horizon.
//
// Items qualify as selectable when they carry either an `id` string
// OR a `title` string — `id` wins as the option value (matches the
// downstream agent's expectations) and `title` is the fallback for
// LLM-produced items that don't have a board-issued id yet.
function findSelectableItems(
  questions: Record<string, unknown>,
  selfName: string,
): QuestionOption[] | null {
  // Pass 1: direct sibling arrays.
  for (const [k, v] of Object.entries(questions)) {
    if (k === selfName) continue;
    if (!Array.isArray(v) || v.length === 0) continue;
    if (!v.every(looksLikeSelectable)) continue;
    return (v as Array<Record<string, unknown>>).map(toOption);
  }
  // Pass 2: descend into sibling objects. Collect arrays of card-
  // shaped items across all eligible nested fields and concatenate
  // them so a single checkbox column covers multiple horizons (e.g.
  // long_term + short_term + next_action under `roadmap`). The
  // accumulator is across nested fields of one sibling only — we
  // don't merge items from two different siblings because that risks
  // mixing semantically different domains.
  for (const [k, v] of Object.entries(questions)) {
    if (k === selfName) continue;
    if (!isPlainObject(v)) continue;
    const collected: Array<Record<string, unknown>> = [];
    let usable = true;
    for (const inner of Object.values(v as Record<string, unknown>)) {
      if (Array.isArray(inner)) {
        if (inner.length === 0) continue;
        if (!inner.every(looksLikeSelectable)) {
          usable = false;
          break;
        }
        collected.push(...(inner as Array<Record<string, unknown>>));
        continue;
      }
      // Singular "next_action"-shaped sub-object: treat as a length-1
      // array iff it independently looks selectable.
      if (looksLikeSelectable(inner)) {
        collected.push(inner as Record<string, unknown>);
      }
      // Anything else (strings, numbers, foreign objects) is just
      // ignored — these are typically explanatory siblings like
      // `rationale: string`.
    }
    if (!usable || collected.length === 0) continue;
    return collected.map(toOption);
  }
  return null;
}

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return (
    typeof v === "object" &&
    v !== null &&
    !Array.isArray(v) &&
    Object.getPrototypeOf(v) === Object.prototype
  );
}

function looksLikeSelectable(v: unknown): boolean {
  if (!isPlainObject(v)) return false;
  const idOk =
    typeof v.id === "string" && (v.id as string).length > 0;
  const titleOk =
    typeof v.title === "string" && (v.title as string).length > 0;
  return idOk || titleOk;
}

function toOption(it: Record<string, unknown>): QuestionOption {
  // Prefer `id` as the option value when present (canonical for
  // board issues) — falls back to `title` when items only carry
  // human-readable identifiers, e.g. LLM-emitted roadmap_items
  // before emit_action assigns board ids.
  const value =
    typeof it.id === "string" && (it.id as string).length > 0
      ? (it.id as string)
      : (it.title as string);
  const titleSrc =
    typeof it.title === "string"
      ? it.title
      : typeof it.name === "string"
        ? it.name
        : "";
  const meta: string[] = [];
  if (typeof it.assignee === "string" && it.assignee.length > 0) {
    meta.push(it.assignee);
  }
  if (typeof it.horizon === "string" && it.horizon.length > 0) {
    meta.push(it.horizon);
  }
  const shortId =
    typeof it.id === "string"
      ? (it.id as string).replace(/^native:/, "").slice(0, 8)
      : "";
  let label = titleSrc || value;
  if (shortId) label += ` · ${shortId}`;
  return {
    value,
    label,
    description: meta.length > 0 ? meta.join(" · ") : undefined,
  };
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
