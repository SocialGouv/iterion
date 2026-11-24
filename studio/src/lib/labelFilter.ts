// The repo's issue-label filter, as the server stores it and the webhook
// lanes read it.
//
// Mirror of pkg/forge.RepoIntegration.LabelAllowlist: a non-empty list narrows
// the issue-labeled lane to those labels (pkg/webhooks.MatchLabel — trimmed,
// case-insensitive, a bare `*` allows all); an empty or absent one lets every
// freshly applied label through to the bots that react to it. Pull requests
// and slash-commands never read it.
//
// The two repo-bots writes (pkg/server forgeEnableReq / forgeUpdateReq) read
// the field three ways: ABSENT keeps the stored list, an EMPTY list clears it,
// a non-empty one replaces it. Every studio site that shows or writes the
// filter reads this module, so the sites agree with each other on the list
// the INTEGRATION holds. A filter set on the repo's webhook alone is not in
// that list and is not shown.

/** The filter an integration holds — empty when it narrows nothing. */
export function storedLabelFilter(
  integration?: { label_allowlist?: string[] | null } | null,
): string[] {
  return integration?.label_allowlist ?? [];
}

/**
 * What a save states about the filter, given the stored list and the one on
 * screen: `undefined` when they are the same list — the key is then omitted
 * and the stored list stands — else the list on screen, an empty one being
 * the server's clearing form.
 */
export function labelFilterChange(stored: string[], shown: string[]): string[] | undefined {
  const same = stored.length === shown.length && stored.every((label, i) => label === shown[i]);
  return same ? undefined : shown;
}

/**
 * The list the field holds after an edit, read as the filter reads it: one
 * label per comma-separated entry of what the operator just entered.
 *
 * The chip input separates on the comma KEY only, so a text that arrives
 * whole — a pasted "bug, security" — is committed as one chip; stored as one
 * label it matches neither. Each entry of `next` that `prev` did not hold is
 * therefore split on its commas and trimmed. An entry `prev` already held is
 * never split: a label set through the API may hold a comma, and the operator
 * did not type it.
 *
 * Blanks are dropped and a label listed twice is kept once, in its first
 * spelling — the server matches labels case-insensitively. That reading
 * covers the whole list, held entries included: a stored list that holds a
 * blank or a duplicate is rewritten by the first edit of the field.
 */
export function splitNewLabelEntries(prev: string[], next: string[]): string[] {
  const held = new Set(prev);
  const seen = new Set<string>();
  const out: string[] = [];
  for (const entry of next) {
    const labels = held.has(entry) ? [entry] : entry.split(",").map((part) => part.trim());
    for (const label of labels) {
      const key = label.toLowerCase();
      if (!label || seen.has(key)) continue;
      seen.add(key);
      out.push(label);
    }
  }
  return out;
}
