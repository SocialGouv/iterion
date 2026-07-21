// Extracted from LaunchView.tsx to keep that file focused.
// Pure module-level helpers that derive launch-form state from a parsed
// IterDocument. None of them touch React; they are imported by both the
// main LaunchView and its split presentational subcomponents.

import type {
  AttachmentField,
  IterDocument,
  Literal,
  Preset,
  VarField,
} from "@/api/types";

/** Read the workflow's vars (workflow-level if a single workflow is
 *  declared, else the file-level `vars:` block). */
export function pickVars(doc: IterDocument | null): VarField[] {
  if (!doc) return [];
  const wf = doc.workflows?.[0];
  if (wf?.vars?.fields?.length) return wf.vars.fields;
  return doc.vars?.fields ?? [];
}

/** Read the workflow's presets (top-level only — they apply to the
 *  whole file, no workflow-level scope today). */
export function pickPresets(doc: IterDocument | null): Preset[] {
  return doc?.presets?.entries ?? [];
}

/** Stringify a preset literal so it can feed the existing var form
 *  (which holds every value as a string and coerces server-side). */
export function literalToString(lit: Literal | undefined): string {
  if (!lit) return "";
  switch (lit.kind) {
    case "string":
      return lit.str_val ?? "";
    case "int":
      return String(lit.int_val ?? 0);
    case "float":
      return String(lit.float_val ?? 0);
    case "bool":
      return lit.bool_val ? "true" : "false";
    default:
      return lit.raw ?? "";
  }
}

/** Read the workflow's attachments — same precedence as vars. */
export function pickAttachments(doc: IterDocument | null): AttachmentField[] {
  if (!doc) return [];
  const wf = doc.workflows?.[0];
  if (wf?.attachments?.fields?.length) return wf.attachments.fields;
  return doc.attachments?.fields ?? [];
}

/** isSandboxActive mirrors the engine's pickMode default: sandboxing is
 *  the DEFAULT, so an ABSENT sandbox block resolves to `auto` and counts
 *  as active. Only an explicit `sandbox: none` (or an unknown mode)
 *  runs the tools on the host. */
export function isSandboxActive(doc: IterDocument | null): boolean {
  const sb = doc?.workflows?.[0]?.sandbox;
  if (!sb) return true;
  const m = (sb.mode ?? "").toLowerCase();
  return m === "auto" || m === "inline" || m === "";
}

/** sandboxModeLabel returns "auto" / "inline" / "none" /
 *  "auto (default)" — the latter when no block is declared and the
 *  engine's sandbox-by-default applies. Used by the SandboxBadge so the
 *  badge label tracks the IR's view of the workflow without re-parsing. */
export function sandboxModeLabel(doc: IterDocument | null): string {
  const sb = doc?.workflows?.[0]?.sandbox;
  if (!sb) return "auto (default)";
  return (sb.mode ?? "").toLowerCase();
}
