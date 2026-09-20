/**
 * The bot navigation vocabulary — studio-side mirror of the Go source of
 * truth in pkg/bundle/vocab.go (BotCategories + KnownBotTags). A Go test
 * (bots/catalog_taxonomy_test.go) asserts slug/tag parity AND order; edit
 * both together. The category set is CLOSED: a seventh slug is a product
 * decision, not an opportunistic edit. The tag seed is OPEN: reusing an
 * existing tag beats inventing one (the label-vocabulary lesson).
 */
export interface BotCategory {
  slug: string;
  title: string;
  tagline: string;
}

// Canonical display order reads as a lifecycle:
// create → judge → strengthen → explain → run → decide.
export const BOT_CATEGORIES: readonly BotCategory[] = [
  { slug: "build", title: "Build", tagline: "ship new capability" },
  { slug: "verify", title: "Verify", tagline: "judge the code, touch nothing" },
  { slug: "harden", title: "Harden", tagline: "strengthen what exists" },
  { slug: "document", title: "Document", tagline: "align words with code" },
  { slug: "operate", title: "Operate", tagline: "run the delivery machinery" },
  { slug: "steer", title: "Steer", tagline: "judge the direction, converse" },
];

// Bots with no declared (or an unknown) category land here, rendered LAST
// and visibly — an unclassified bot is never hidden.
export const UNCATEGORIZED = {
  slug: "",
  title: "Uncategorized",
  tagline: "visible, never hidden",
} satisfies BotCategory;

export const KNOWN_BOT_TAGS: readonly string[] = [
  "ships-code",
  "read-only",
  "code-review",
  "security",
  "supply-chain",
  "deps",
  "upgrade",
  "docs",
  "tests",
  "a11y",
  "observability",
  "architecture",
  "strategy",
  "planning",
  "conversational",
  "scheduled",
  "board",
  "deploy",
  "env",
  "tooling",
  "git",
  "triage",
  "human-in-the-loop",
  "programme",
];

// groupBotsByCategory buckets bots into canonical category order with the
// Uncategorized group last. All six categories are returned (stable
// landmarks, even empty); Uncategorized only when non-empty.
export function groupBotsByCategory<T extends { category?: string }>(
  bots: readonly T[],
): { category: BotCategory; bots: T[] }[] {
  const bySlug = new Map<string, T[]>();
  for (const b of bots) {
    const slug = BOT_CATEGORIES.some((c) => c.slug === b.category)
      ? (b.category as string)
      : "";
    const list = bySlug.get(slug) ?? [];
    list.push(b);
    bySlug.set(slug, list);
  }
  const groups = BOT_CATEGORIES.map((category) => ({
    category,
    bots: bySlug.get(category.slug) ?? [],
  }));
  const uncat = bySlug.get(UNCATEGORIZED.slug) ?? [];
  if (uncat.length > 0) {
    groups.push({ category: UNCATEGORIZED, bots: uncat });
  }
  return groups;
}
