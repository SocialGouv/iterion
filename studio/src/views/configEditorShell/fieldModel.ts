// ---------------------------------------------------------------------------
// Editable-field model — walk the server's projected config into a flat list
// of editable string / string-array leaves.
// ---------------------------------------------------------------------------

type FieldKind = "string" | "array";
export type FieldValue = string | string[];

export interface EditableField {
  /** Dotted path from the config root, e.g. "categories.a11y.feeds". */
  path: string;
  /** Last path segment, used as the field label ("feeds", "editorial"). */
  leaf: string;
  /** Dotted parent path ("categories › a11y"), "" for a top-level leaf. */
  parentLabel: string;
  kind: FieldKind;
}

function isStringArray(v: unknown): v is string[] {
  return Array.isArray(v) && v.every((x) => typeof x === "string");
}

// walkEditableFields does a stable depth-first walk of the projection,
// collecting every string leaf and every (all-string) array leaf. Nested
// objects are recursed; numbers/booleans/null and arrays-of-objects are
// skipped (nothing to render an input for). The server has already projected
// `config` down to the editable surface, so in practice every leaf here is
// something the PATCH accepts.
export function walkEditableFields(
  config: Record<string, unknown>,
  prefix: string[] = [],
): EditableField[] {
  const out: EditableField[] = [];
  for (const key of Object.keys(config)) {
    const value = config[key];
    const path = [...prefix, key];
    if (typeof value === "string") {
      out.push(fieldAt(path, "string"));
    } else if (isStringArray(value)) {
      out.push(fieldAt(path, "array"));
    } else if (value && typeof value === "object" && !Array.isArray(value)) {
      out.push(...walkEditableFields(value as Record<string, unknown>, path));
    }
    // else: not an editable leaf — skip.
  }
  return out;
}

function fieldAt(path: string[], kind: FieldKind): EditableField {
  const leaf = path[path.length - 1] ?? "";
  const parent = path.slice(0, -1);
  return {
    path: path.join("."),
    leaf,
    parentLabel: parent.join(" › "),
    kind,
  };
}

function getPath(obj: Record<string, unknown>, path: string): unknown {
  let cur: unknown = obj;
  for (const p of path.split(".")) {
    if (cur === null || typeof cur !== "object") return undefined;
    cur = (cur as Record<string, unknown>)[p];
  }
  return cur;
}

function setPath(target: Record<string, unknown>, path: string, value: unknown): void {
  const parts = path.split(".");
  const last = parts.length - 1;
  let cur: Record<string, unknown> = target;
  for (let i = 0; i < last; i++) {
    const p = parts[i];
    if (p === undefined) continue;
    const next = cur[p];
    if (next === null || typeof next !== "object" || Array.isArray(next)) {
      cur[p] = {};
    }
    cur = cur[p] as Record<string, unknown>;
  }
  const leaf = parts[last];
  if (leaf !== undefined) cur[leaf] = value;
}

function readStringAt(config: Record<string, unknown>, path: string): string {
  const v = getPath(config, path);
  return typeof v === "string" ? v : "";
}

function readArrayAt(config: Record<string, unknown>, path: string): string[] {
  const v = getPath(config, path);
  return isStringArray(v) ? v.slice() : [];
}

export type Draft = Record<string, FieldValue>;

// initDraft projects the config into the form's editable values. Empty arrays
// default to a single blank row so a first-time editor sees a field to type in.
export function initDraft(fields: EditableField[], config: Record<string, unknown>): Draft {
  const draft: Draft = {};
  for (const f of fields) {
    if (f.kind === "string") {
      draft[f.path] = readStringAt(config, f.path);
    } else {
      const arr = readArrayAt(config, f.path);
      draft[f.path] = arr.length > 0 ? arr : [""];
    }
  }
  return draft;
}

export function normArray(a: string[]): string[] {
  return a.map((s) => s.trim()).filter((s) => s.length > 0);
}

function arraysEqual(a: string[], b: string[]): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false;
  return true;
}

export function fieldChanged(field: EditableField, draft: Draft, baseline: Draft): boolean {
  const d = draft[field.path];
  const b = baseline[field.path];
  if (field.kind === "string") return (d as string) !== (b as string);
  return !arraysEqual(normArray(d as string[]), normArray(b as string[]));
}

// buildPatch turns the changed leaves into a sparse nested-object PATCH body.
// Strings are sent as-is (whitespace can be meaningful); arrays are trimmed
// with empty rows dropped.
export function buildPatch(
  fields: EditableField[],
  draft: Draft,
  baseline: Draft,
): Record<string, unknown> {
  const patch: Record<string, unknown> = {};
  for (const f of fields) {
    if (!fieldChanged(f, draft, baseline)) continue;
    const value =
      f.kind === "string" ? (draft[f.path] as string) : normArray(draft[f.path] as string[]);
    setPath(patch, f.path, value);
  }
  return patch;
}

// Branding is the bot-declared editor title/description, surfaced once the
// share list loads so the header + heading can read "Éditeur de veilles"
// instead of the generic "Config editor".
export interface Branding {
  title?: string;
  description?: string;
}
