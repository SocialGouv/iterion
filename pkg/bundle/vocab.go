package bundle

import "strings"

// BotCategory is one entry of the CLOSED vocabulary a bot's manifest
// `category:` may declare — the spine of every bot navigation surface
// (studio gallery, `iterion bots list`, the generated orchestrator
// catalog). Six verbs, one canonical order that reads as a lifecycle:
// create → judge → strengthen → explain → run → decide. Adding a seventh
// slug is a product decision documented in docs/agents/bot-authoring.md,
// never an opportunistic manifest edit.
type BotCategory struct {
	// Slug is the manifest value (`category: verify`).
	Slug string
	// Title is the UI header word ("Verify").
	Title string
	// Tagline is the one-line description rendered under the header.
	Tagline string
}

// BotCategories is the closed category set, in canonical display order.
// A manifest declaring an unknown slug is never rejected and never
// rewritten — the bot lands in the "Uncategorized" group, visibly last,
// and bundlelint emits a soft diagnostic naming the known slugs.
var BotCategories = []BotCategory{
	{Slug: "build", Title: "Build", Tagline: "ship new capability"},
	{Slug: "verify", Title: "Verify", Tagline: "judge the code, touch nothing"},
	{Slug: "harden", Title: "Harden", Tagline: "strengthen what exists"},
	{Slug: "document", Title: "Document", Tagline: "align words with code"},
	{Slug: "operate", Title: "Operate", Tagline: "run the delivery machinery"},
	{Slug: "steer", Title: "Steer", Tagline: "judge the direction, converse"},
}

// UncategorizedSlug names the group a bot with no `category:` (or an
// unknown one) falls into. It is rendered LAST on every surface: an
// unclassified bot stays visible, never hidden.
const UncategorizedSlug = ""

// BotCategoryBySlug returns the category with the given slug.
func BotCategoryBySlug(slug string) (BotCategory, bool) {
	for _, c := range BotCategories {
		if c.Slug == slug {
			return c, true
		}
	}
	return BotCategory{}, false
}

// IsKnownBotTag reports whether the tag is in the curated seed.
func IsKnownBotTag(tag string) bool {
	for _, t := range KnownBotTags {
		if t == tag {
			return true
		}
	}
	return false
}

// KnownBotTagsJoined joins the curated seed for lint messages.
func KnownBotTagsJoined() string {
	return strings.Join(KnownBotTags, ", ")
}

// KnownBotCategorySlugs joins the known slugs for lint messages.
func KnownBotCategorySlugs() string {
	slugs := make([]string, 0, len(BotCategories))
	for _, c := range BotCategories {
		slugs = append(slugs, c.Slug)
	}
	return strings.Join(slugs, ", ")
}

// normalizeBotCategory normalizes the FORM of a declared category
// (trim + lowercase) without touching its VALUE: an unknown slug stays
// declared, visibly, for lint and the Uncategorized group to name.
func normalizeBotCategory(c string) string {
	return strings.ToLower(strings.TrimSpace(c))
}

// normalizeBotTagList trims and lowercases each tag, drops empties, and
// dedupes keeping first-occurrence order. Returns nil when nothing
// survives, so an effectively-empty tag list serialises as absent.
func normalizeBotTagList(tags []string) []string {
	var out []string
	seen := make(map[string]bool, len(tags))
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// KnownBotTags is the curated seed of the OPEN tag vocabulary a bot's
// manifest `tags:` draws from. Tags are orthogonal facets (domain,
// safety, modality) — the category is the intent spine, tags are how
// views slice across it. The set is deliberately open: a new tag is a
// lint warning (reuse before inventing), never a rejection. This list
// mirrors studio/src/lib/botTaxonomy.ts — update both together.
var KnownBotTags = []string{
	// Safety — the launch-decision facet: does it touch the tree?
	"ships-code",
	"read-only",
	// Domains
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
	// Modality / surface
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
}
