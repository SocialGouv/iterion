// The bot catalog the orchestrator routes on is refreshed by the server
// whenever a bundle is installed, updated or removed. That refresh is
// non-fatal — the gesture itself already landed — so its failure travels on
// the result instead, and every surface that confirms the gesture shows it.

/** CatalogRefreshed is any API result carrying the outcome of that refresh. */
export interface CatalogRefreshed {
  /** Rendered cause of a refresh that failed; absent when it succeeded. */
  catalog_error?: string;
}

/** catalogStaleSuffix renders what the operator must read next to a
 *  successful install, update or removal whose catalog refresh failed: the
 *  workspace holds what they asked for, but the catalog still advertises the
 *  previous set until a regeneration succeeds. Returns "" when the refresh
 *  succeeded, so a caller appends it unconditionally. */
export function catalogStaleSuffix(res: CatalogRefreshed | null | undefined): string {
  const cause = res?.catalog_error?.trim();
  if (!cause) return "";
  return ` — catalog not refreshed: ${cause} (run \`iterion bots regen-catalog\`)`;
}

/** catalogToggleToast renders what a catalog-affecting gesture confirms —
 *  a visibility toggle (the Catalog manager's quick switch, the bot
 *  header's), an icon save, an overlay reset: the state the bot is in now,
 *  plus — when that gesture's catalog refresh failed — the fact the
 *  routing table still shows the previous set. A toggle decides which rows
 *  the table holds; an icon save or reset what they render from. Either
 *  way, a failed refresh means the state just confirmed is not what the
 *  table shows yet. */
export function catalogToggleToast(state: string, res: CatalogRefreshed | null | undefined): string {
  return `${state}${catalogStaleSuffix(res)}`;
}
