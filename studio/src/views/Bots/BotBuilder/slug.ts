// Pure helpers for the bot builder — kept React-free so they are unit
// testable and reusable (the builder derives the bundle slug live from
// the display name field).

/** Mirrors the server-side botscaffold slug rule (pkg/botscaffold):
 *  lowercase letter first, then lowercase letters / digits / dashes,
 *  2–64 chars total. */
export const SLUG_RE = /^[a-z][a-z0-9-]{1,63}$/;

/**
 * deriveSlug turns a human display name into the bot's directory slug:
 * lowercase, accents folded, spaces/underscores → dashes, every other
 * invalid character stripped, dash runs collapsed, leading digits or
 * dashes trimmed (a slug must start with a letter), trailing dashes
 * trimmed, capped at 64 chars. The result may still fail SLUG_RE
 * (e.g. empty or single-char input) — callers surface validity inline.
 */
export function deriveSlug(name: string): string {
  return name
    .toLowerCase()
    // Fold accented letters to their base (é → e) so "Résumé bot"
    // derives a usable slug instead of dropping the letters entirely.
    .normalize("NFKD")
    .replace(/[\u0300-\u036f]/g, "")
    .replace(/[\s_]+/g, "-")
    .replace(/[^a-z0-9-]/g, "")
    .replace(/-+/g, "-")
    .replace(/^[0-9-]+/, "")
    .slice(0, 64)
    .replace(/-+$/, "");
}

export function isValidSlug(slug: string): boolean {
  return SLUG_RE.test(slug);
}

/** Workflow var-name rule enforced inline by the vars editor. */
export const VAR_NAME_RE = /^[a-z_][a-z0-9_]*$/;

export function isValidVarName(name: string): boolean {
  return VAR_NAME_RE.test(name);
}
