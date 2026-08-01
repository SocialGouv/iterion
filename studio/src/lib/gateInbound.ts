// Typing of a human gate's INBOUND payload — the data the operator is
// being asked to validate.
//
// Where it comes from: `Engine.persistPause` resolves the paused node's
// incoming edges (`with { plan: "{{outputs.plan.body}}" }`) and stores
// the result verbatim as `store.Interaction.Questions`. That map reaches
// the browser on every paused gate — as the `human_input_requested`
// event payload in the run console, and as
// `PipelineBoardPendingReview.Questions` on the board.
//
// Why this file exists: the answer form is driven by the node's OUTPUT
// schema (the verdict the operator produces), so nothing rendered the
// inbound half. A gate reviewing a generated plan, a diff or a rendered
// mockup had to stringify it into `instructions:` or the operator
// answered blind (iterion#332).
//
// The kind is resolved from the node's declared `input_schema` when it
// has one, and inferred from the value's shape otherwise — most gates
// declare no input schema, and "no schema" must not mean "no rendering".

import type { WireSchemaField } from "@/api/runs";
import { ASK_USER_RESPONSE_KEY, isReservedQuestionKey } from "@/lib/askUserOptions";

/** How one inbound value should be presented. */
export type GateInboundKind =
  | "markdown" // prose / long text — rendered through MarkdownText
  | "json" // object, array, or a `json`-typed field — pretty-printed, collapsible
  | "file" // an operator upload / attachment descriptor — previewed
  | "scalar"; // bool, number, short single-line string — inline

/** A file value, normalised across the two shapes the engine emits. */
export interface GateInboundFileRef {
  /** Run-attachment name (`{attachment: "gate.mockup"}`), when present. */
  attachment?: string;
  /** Filesystem path the RUNNING NODES see. Never fetchable from a browser. */
  path?: string;
  filename?: string;
  mime?: string;
  size?: number;
}

export interface GateInboundItem {
  /** The `with {}` key — also the display label. */
  key: string;
  kind: GateInboundKind;
  /** Raw value, untouched. */
  value: unknown;
  /** Populated iff kind === "file". */
  file?: GateInboundFileRef;
  /** Populated iff kind === "markdown" | "scalar" — the text to render. */
  text?: string;
  /** True when the schema declared the kind; false when it was inferred. */
  typed: boolean;
}

// A single-line string this short reads better inline than as a
// markdown block; above it, prose formatting earns its keep.
const SCALAR_MAX_LEN = 120;

// Engine-synthesised question keys that are the ASK itself, not data the
// gate received. Both are already rendered as the turn's prompt (run
// console) or as the answerable field (PauseForm); repeating them as
// "review context" would just double them up.
//
//   - ask_user_response — the agent's own question on an ask_user pause.
//     Not underscore-prefixed for wire-compat reasons.
//   - acknowledge_recovery — the graceful-failure pause's guidance
//     ("re-authenticate, then resume…"), written by
//     pkg/runtime/recovery_dispatch.go.
const SYNTHETIC_QUESTION_KEYS = new Set([ASK_USER_RESPONSE_KEY, "acknowledge_recovery"]);

/**
 * Keys that are runtime plumbing rather than gate content.
 *
 * `_`-prefixed keys are the reserved family (queued operator messages,
 * ask_user options, the permission marker, ad-hoc attachments), plus the
 * synthetic asks above.
 */
export function isGatePlumbingKey(key: string): boolean {
  return isReservedQuestionKey(key) || SYNTHETIC_QUESTION_KEYS.has(key);
}

/**
 * Normalise a value into a file reference, or null when it is not one.
 *
 * Two legitimate shapes (mirrors pkg/backend/model/validate.go):
 *   - the descriptor the resume path writes after promoting an upload:
 *     `{attachment, path, filename, mime, size, sha256}`
 *   - a bare path string (an LLM-answered gate, or `--answer x=@file`)
 *
 * A bare string is only read as a file when the SCHEMA says so — a
 * gate's prose field would otherwise be mistaken for a path.
 */
export function asFileRef(value: unknown, declaredFile: boolean): GateInboundFileRef | null {
  if (typeof value === "string") {
    const path = value.trim();
    if (!declaredFile || !path) return null;
    return { path, filename: basename(path) };
  }
  if (!value || typeof value !== "object" || Array.isArray(value)) return null;
  const desc = value as Record<string, unknown>;
  const attachment = typeof desc.attachment === "string" ? desc.attachment : undefined;
  const path = typeof desc.path === "string" ? desc.path : undefined;
  // An attachment name alone is not enough: a legitimate structured
  // payload can carry an `attachment` key of its own (a triage item
  // naming a screenshot), and misreading it as a descriptor would
  // replace the data the gate exists to show with a 404 banner. Real
  // descriptors always carry corroboration — filename/mime/size from the
  // HTTP promotion path, path from the engine — so requiring it costs
  // nothing.
  const corroborated =
    !!attachment &&
    (declaredFile || "path" in desc || "mime" in desc || "size" in desc || "sha256" in desc);
  if (!corroborated && !(declaredFile && path)) return null;
  const filename =
    typeof desc.filename === "string" && desc.filename
      ? desc.filename
      : path
        ? basename(path)
        : attachment;
  return {
    attachment,
    path,
    filename,
    mime: typeof desc.mime === "string" ? desc.mime : undefined,
    size: typeof desc.size === "number" ? desc.size : undefined,
  };
}

function basename(p: string): string {
  const cut = Math.max(p.lastIndexOf("/"), p.lastIndexOf("\\"));
  return cut >= 0 ? p.slice(cut + 1) : p;
}

/**
 * Build the ordered render list for a gate's inbound payload.
 *
 * Ordering follows the declared input schema first (the author's own
 * order, which is the reading order they intended), then any remaining
 * keys alphabetically — an undeclared payload is otherwise at the mercy
 * of JSON key order, which is stable per run but arbitrary across bots.
 *
 * Values that carry nothing (null, undefined, empty string, empty
 * object/array) are dropped: a `with {}` mapping resolving to nil is a
 * valid mapping the engine deliberately keeps, but an empty row is
 * noise, not context.
 */
export function gateInboundItems(
  questions: Record<string, unknown> | null | undefined,
  inputFields: WireSchemaField[] | null | undefined,
  // Keys the node's `instructions:` prompt already interpolates
  // (`{{input.<key>}}`, projected as WireNode.instruction_inputs). The
  // engine substitutes them into the operator-facing instructions
  // rendered right above this block, so repeating them here would show
  // the same plan/reply/PRD twice — the exact duplication this feature
  // exists to REMOVE.
  alreadyShown?: readonly string[] | null,
): GateInboundItem[] {
  if (!questions) return [];
  const consumed = new Set(alreadyShown ?? []);
  const typeByName = new Map<string, string>();
  for (const f of inputFields ?? []) typeByName.set(f.name, f.type);

  const declaredOrder = (inputFields ?? []).map((f) => f.name);
  const rest = Object.keys(questions)
    .filter((k) => !declaredOrder.includes(k))
    .sort();
  const keys = [...declaredOrder.filter((k) => k in questions), ...rest];

  const items: GateInboundItem[] = [];
  for (const key of keys) {
    if (isGatePlumbingKey(key) || consumed.has(key)) continue;
    const value = questions[key];
    if (isEmptyValue(value)) continue;
    const declared = typeByName.get(key);
    items.push(classify(key, value, declared));
  }
  return items;
}

function isEmptyValue(value: unknown): boolean {
  if (value === null || value === undefined) return true;
  if (typeof value === "string") return value.trim() === "";
  if (Array.isArray(value)) return value.length === 0;
  if (typeof value === "object") return Object.keys(value as object).length === 0;
  return false;
}

function classify(key: string, value: unknown, declared: string | undefined): GateInboundItem {
  const typed = declared !== undefined;
  const file = asFileRef(value, declared === "file");
  if (file) return { key, kind: "file", value, file, typed };

  if (typeof value === "string") {
    // A `json`-typed field whose value arrived as a JSON string is still
    // structured data to the operator — parse it back so it renders as
    // such instead of as a wall of escaped text.
    if (declared === "json") {
      const parsed = tryParseJSON(value);
      if (parsed !== undefined) return { key, kind: "json", value: parsed, typed };
    }
    const oneLine = !value.includes("\n");
    if (oneLine && value.length <= SCALAR_MAX_LEN) {
      return { key, kind: "scalar", value, text: value, typed };
    }
    return { key, kind: "markdown", value, text: value, typed };
  }

  if (typeof value === "boolean" || typeof value === "number") {
    return { key, kind: "scalar", value, text: String(value), typed };
  }

  return { key, kind: "json", value, typed };
}

function tryParseJSON(raw: string): unknown {
  const trimmed = raw.trim();
  if (!trimmed.startsWith("{") && !trimmed.startsWith("[")) return undefined;
  try {
    return JSON.parse(trimmed);
  } catch {
    return undefined;
  }
}

/** Stable pretty-printed form for a `json` item (never throws). */
export function formatJSONValue(value: unknown): string {
  try {
    return JSON.stringify(value, null, 2) ?? String(value);
  } catch {
    return String(value);
  }
}
