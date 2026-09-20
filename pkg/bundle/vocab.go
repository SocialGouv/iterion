package bundle

import (
	"slices"
	"strings"
)

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

// UncategorizedCategory is the display metadata of that trailing group —
// declared HERE, next to the closed set, so no renderer re-authors the
// title or tagline (the drift this replaces shipped once already).
var UncategorizedCategory = BotCategory{
	Slug:    UncategorizedSlug,
	Title:   "Uncategorized",
	Tagline: "visible, never hidden",
}

// CategoryGroup is one category section of a grouped view: the entries
// whose declared category matches Category.Slug, in input order.
type CategoryGroup[T any] struct {
	Category BotCategory
	Bots     []T
}

// IsUncategorizedSlug reports whether a DECLARED slug lands in the
// Uncategorized group on every grouped surface — no category at all, or
// one the closed set does not know. The FILTERS use the same predicate
// (`--category uncategorized`, `?category=uncategorized`): a filter and
// the grouping it navigates must never disagree about who is in the
// group.
func IsUncategorizedSlug(slug string) bool {
	if slug == "" {
		return true
	}
	_, ok := BotCategoryBySlug(slug)
	return !ok
}

// GroupByCategory buckets entries into the canonical category order
// (BotCategories) with the Uncategorized group appended last. The seam
// owns FORM: an accessor value is normalized (trim + lowercase) before
// placement, so an entry built outside the manifest loader groups the
// same as one that went through it. Every group is returned — an empty
// canonical category is a landmark worth keeping; the renderer decides
// whether to print it (a routing document skips empties, fixed-landmark
// UIs show them). This is the ONE grouping implementation: display
// surfaces consume it with a one-line accessor, they never re-derive the
// order or the placement.
func GroupByCategory[T any](entries []T, categoryOf func(T) string) []CategoryGroup[T] {
	groups := make([]CategoryGroup[T], 0, len(BotCategories)+1)
	for _, c := range BotCategories {
		groups = append(groups, CategoryGroup[T]{Category: c})
	}
	groups = append(groups, CategoryGroup[T]{Category: UncategorizedCategory})
	for _, e := range entries {
		category := normalizeBotCategory(categoryOf(e))
		placed := false
		for i := range BotCategories {
			if category == BotCategories[i].Slug {
				groups[i].Bots = append(groups[i].Bots, e)
				placed = true
				break
			}
		}
		if !placed {
			last := len(groups) - 1
			groups[last].Bots = append(groups[last].Bots, e)
		}
	}
	return groups
}

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
	return slices.Contains(KnownBotTags, tag)
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

// NormalizeBotCategory is the form normalization applied to a declared
// category AND to a `--category` filter value — one function so both
// always meet in the same form.
func NormalizeBotCategory(c string) string { return normalizeBotCategory(c) }

// NormalizeBotTagList is normalizeBotTagList exported for filter values;
// its dedupe is harmless on filter input (a repeated --tag narrows the
// same way).
func NormalizeBotTagList(tags []string) []string { return normalizeBotTagList(tags) }

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
	return normalizeStringList(tags, true)
}

// KnownBotTags is the curated seed of the OPEN tag vocabulary a bot's
// manifest `tags:` draws from. Tags are orthogonal facets (domain,
// safety, modality) — the category is the intent spine, tags are how
// views slice across it. The set is deliberately open: a new tag is a
// lint warning (reuse before inventing), never a rejection.
//
// The safety pair is the launch-decision facet and has a WRITTEN
// boundary: `ships-code` = the bot's run commits into the TARGET repo's
// history (source, docs, wiki, config files, versioned state — anything
// a `git log` will show); `read-only` = the run writes nothing anywhere
// (verdicts go to the board/chat, never a commit). Documented in
// docs/agents/bot-authoring.md; the fleet gate holds the annotations to
// this boundary.
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
