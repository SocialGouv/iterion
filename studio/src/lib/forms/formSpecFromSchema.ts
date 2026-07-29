import type { WireSchemaField } from "@/api/runs";
import { UPLOAD_ENVELOPE_KEY } from "@/api/runs/types";
import { humanizeKey } from "@/lib/humanizeKey";
import { shortIssueId } from "@/lib/whats-next/issueId";
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
  const hasEnum = !!(field.enum_values && field.enum_values.length > 0);
  const base = {
    id: field.name,
    // Humanise the raw schema field name (selected_story_ids →
    // "Selected story ids") so the operator reads a label, not an
    // identifier — shared with the output-card renderer so both
    // surfaces agree. The author's `instructions:` carries the real
    // per-field explanation above the form. (A first-class `label:`
    // schema primitive would supersede this fallback — see the
    // human-form follow-ups.)
    label: humanizeKey(field.name),
    description,
    // A constrained choice (enum radio/select or a bool) is a decision
    // the operator must make; free-form values (text, numbers, JSON,
    // lists) are elaboration and stay optional — so an "approve" no
    // longer forces a feedback note. The json multi-select branch
    // below additionally pins required:false.
    required: hasEnum || field.type === "bool",
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
    case "file":
      return {
        ...base,
        kind: "file",
        // Optional, like every other free-form field: the DSL has no
        // per-field `required` marker, and forcing a file would strand
        // an operator re-answering a gate whose file was already
        // supplied on an earlier pass. A workflow that truly cannot
        // proceed without one branches on the field downstream.
        required: false,
        // Best-effort picker filter derived from the field name — a
        // field called `music` or `soundtrack` should not make the
        // operator wade through every file on disk. Purely advisory:
        // the server sniffs and enforces the real MIME allowlist.
        accept: acceptForFieldName(field.name),
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

// coerceMaybeJson parses a value that arrived as a JSON-encoded string.
// iterion stores `json`-typed agent output fields as their raw JSON text
// (correct for prompt rendering — `{{input.stories}}` must hand the LLM
// JSON, not "[object Object]"), so a real agent's stories/created_issues
// reach the form as a string while a --var-supplied one arrives already
// parsed. The checkbox detection needs the array either way. Returns the
// parsed value when the string is a JSON array/object, else v unchanged.
function coerceMaybeJson(v: unknown): unknown {
  if (typeof v !== "string") return v;
  const s = v.trim();
  if (s.length < 2 || (s[0] !== "[" && s[0] !== "{")) return v;
  try {
    return JSON.parse(s);
  } catch {
    return v;
  }
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
    const cv = coerceMaybeJson(v);
    if (!Array.isArray(cv) || cv.length === 0) continue;
    if (!cv.every(looksLikeSelectable)) continue;
    return cv.map(toOption);
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
    const cv = coerceMaybeJson(v);
    if (!isPlainObject(cv)) continue;
    const collected: Array<Record<string, unknown>> = [];
    let usable = true;
    for (const innerRaw of Object.values(cv)) {
      const inner = coerceMaybeJson(innerRaw);
      if (Array.isArray(inner)) {
        if (inner.length === 0) continue;
        if (!inner.every(looksLikeSelectable)) {
          usable = false;
          break;
        }
        collected.push(...inner);
        continue;
      }
      // Singular "next_action"-shaped sub-object: treat as a length-1
      // array iff it independently looks selectable.
      if (looksLikeSelectable(inner)) {
        collected.push(inner);
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

function looksLikeSelectable(v: unknown): v is Record<string, unknown> {
  if (!isPlainObject(v)) return false;
  const idOk = typeof v.id === "string" && v.id.length > 0;
  const titleOk = typeof v.title === "string" && v.title.length > 0;
  return idOk || titleOk;
}

function toOption(it: Record<string, unknown>): QuestionOption {
  // Prefer `id` as the option value when present (canonical for
  // board issues) — falls back to `title` when items only carry
  // human-readable identifiers, e.g. LLM-emitted roadmap_items
  // before emit_action assigns board ids. looksLikeSelectable
  // guarantees at least one of id/title is a non-empty string.
  const id = typeof it.id === "string" ? it.id : "";
  const title = typeof it.title === "string" ? it.title : "";
  const value = id.length > 0 ? id : title;
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
  const shortId = typeof it.id === "string" ? shortIssueId(it.id) : "";
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
    case "file":
      // `raw` is the staged upload id the file picker produced. Wrap it
      // in the envelope the resume endpoint recognises; the server
      // promotes the bytes to a run attachment and swaps the envelope
      // for a descriptor before the engine sees the answers.
      return { value: { [UPLOAD_ENVELOPE_KEY]: raw }, error: null };
    case "string":
    default:
      return { value: raw, error: null };
  }
}

// acceptForFieldName guesses a picker filter from the schema field's
// name. The DSL carries no per-field MIME declaration, and an
// unfiltered picker on a field plainly called `soundtrack` makes the
// operator hunt through every file on disk — the exact chore gate
// uploads exist to remove. Advisory only: guessing wrong costs a
// slightly wider picker, never a rejected file, because the operator
// can always switch the picker to "all files" and the SERVER decides
// what is actually allowed.
export function acceptForFieldName(name: string): string | undefined {
  const n = name.toLowerCase();
  if (/(music|audio|sound|track|song|voice|narration|jingle)/.test(n)) {
    return "audio/*";
  }
  if (/(video|clip|footage|movie|recording)/.test(n)) return "video/*";
  if (/(image|photo|picture|screenshot|logo|thumbnail|cover|diagram|sketch)/.test(n)) {
    return "image/*";
  }
  return undefined;
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
