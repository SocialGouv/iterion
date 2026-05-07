import type { AttachmentField, UploadLimits } from "../api/types";

export interface ValidationResult {
  ok: boolean;
  error?: string;
}

/**
 * Validate a File against the workflow's attachment field declaration
 * and the server's upload limits. Pure — no I/O — so it can run inside
 * onChange handlers and unit tests without async setup.
 *
 * The server is the source of truth (it re-validates everything on
 * upload), but applying these rules client-side gives the user instant
 * feedback before bytes leave the browser.
 */
export function validateAttachment(
  file: File,
  field: AttachmentField,
  limits: UploadLimits | null,
): ValidationResult {
  if (!file) return { ok: false, error: "no file selected" };
  if (file.size === 0) return { ok: false, error: "empty file" };

  const max = limits?.max_file_size ?? 0;
  if (max > 0 && file.size > max) {
    return { ok: false, error: `file too large (max ${formatBytes(max)})` };
  }

  // MIME validation: file.type must satisfy BOTH the field's
  // accept_mime (when declared) AND the server allowlist (when known).
  const declaredAllow = field.accept_mime?.length
    ? field.accept_mime
    : deriveAccept(field.type);
  const serverAllow = limits?.allowed_mime ?? [];

  if (declaredAllow.length > 0 && !declaredAllow.some((p) => mimeMatches(file.type, p))) {
    return {
      ok: false,
      error: `${file.type || "unknown type"} not allowed (expected ${declaredAllow.join(", ")})`,
    };
  }
  if (serverAllow.length > 0 && !serverAllow.some((p) => mimeMatches(file.type, p))) {
    return {
      ok: false,
      error: `${file.type || "unknown type"} not allowed by server (expected ${serverAllow.join(", ")})`,
    };
  }
  return { ok: true };
}

/** Default MIME allowlist for each attachment type. */
export function deriveAccept(type: AttachmentField["type"]): string[] {
  switch (type) {
    case "image":
      return ["image/png", "image/jpeg", "image/webp", "image/gif"];
    case "file":
    default:
      return [];
  }
}

/**
 * mimeMatches tests whether a MIME string matches a glob pattern.
 * Both `image/*` and `image/png` are accepted. Case-insensitive,
 * tolerates `; charset=...` parameters.
 */
export function mimeMatches(mime: string, pattern: string): boolean {
  if (!mime || !pattern) return false;
  const m = mime.toLowerCase().split(";", 1)[0]!.trim();
  const p = pattern.toLowerCase().split(";", 1)[0]!.trim();
  if (p === "*" || p === "*/*") return true;

  const [mt, msub = ""] = m.split("/");
  const [pt, psub = ""] = p.split("/");
  if (pt !== "*" && pt !== mt) return false;
  return psub === "*" || psub === msub;
}

/** Pretty-print bytes (1024-based) for error/UI strings. */
export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const u = ["KB", "MB", "GB", "TB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < u.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v < 10 ? 1 : 0)} ${u[i]}`;
}

/** Sum the size of every selected attachment for the running total. */
export function totalSize(values: Record<string, { file: File } | undefined>): number {
  let s = 0;
  for (const v of Object.values(values)) {
    if (v?.file) s += v.file.size;
  }
  return s;
}
