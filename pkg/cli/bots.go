package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/bundle"
)

// BotEntry is an alias of botregistry.Entry so existing CLI callers and
// tests keep working. Discovery + schema-augmented variants live in
// pkg/botregistry (importable by pkg/server, which cannot import pkg/cli).
type BotEntry = botregistry.Entry

// BotsListOptions configures discovery for [BotsList].
type BotsListOptions struct {
	// Paths is the list of roots to walk. A path may point to a single
	// .bot file (treated as one entry), a .botz bundle directory, or a
	// directory containing many .bot files / sub-bundles.
	Paths []string

	// Format selects the output rendering: "json" (default), "markdown",
	// "skill" (a SKILL.md ready to drop in a `<bundle>/skills/`), or
	// "tree" (the category → bot → presets navigation spine).
	Format string

	// Categories keeps only bots whose declared category matches one of
	// the slugs (form-normalized like the manifest). The pseudo-slug
	// "uncategorized" selects bots with no category. Empty = all.
	Categories []string

	// Tags keeps only bots carrying EVERY listed tag (narrowing — the
	// AND a "view by tag" means). Empty = all.
	Tags []string

	// ErrW, when set, receives one warning line per skipped malformed
	// bundle/bot file (discovery keeps listing the valid siblings). Nil
	// discards the warnings — the structured diagnostics stay available
	// through botregistry.ListWithDiagnostics.
	ErrW io.Writer
}

// BotsList walks Opts.Paths, parses metadata, and writes the result to w.
func BotsList(opts BotsListOptions, w io.Writer) error {
	if len(opts.Paths) == 0 {
		return fmt.Errorf("bots: no paths specified")
	}
	if opts.Format == "" {
		opts.Format = "json"
	}

	warnUnknownTaxonomyValues(opts.ErrW, opts.Categories, opts.Tags)

	switch opts.Format {
	case "json":
		entries, diags, err := botregistry.ListWithDiagnostics(botregistry.ListOptions{Paths: opts.Paths})
		if err != nil {
			return err
		}
		warnDiscoveryErrors(opts.ErrW, diags)
		entries = filterByTaxonomy(entries, func(e BotEntry) botregistry.Entry { return e }, opts.Categories, opts.Tags)
		if entries == nil {
			entries = []BotEntry{} // a filtered-to-empty list is [], not null
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	case "markdown":
		entries, diags, err := botregistry.ListWithDiagnostics(botregistry.ListOptions{Paths: opts.Paths})
		if err != nil {
			return err
		}
		warnDiscoveryErrors(opts.ErrW, diags)
		return renderBotsMarkdown(w, filterByTaxonomy(entries, func(e BotEntry) botregistry.Entry { return e }, opts.Categories, opts.Tags))
	case "skill":
		// The skill catalog wants the per-bot vars too, so use the
		// schema-augmented list and the shared catalog renderer (the same
		// one botregistry.RegenerateWhatsNextCatalog splices into Nexie's
		// live catalog).
		entries, diags, err := botregistry.ListWithSchemaDiagnostics(botregistry.ListOptions{Paths: opts.Paths})
		if err != nil {
			return err
		}
		warnDiscoveryErrors(opts.ErrW, diags)
		return renderBotsSkill(w, filterByTaxonomy(entries, func(e botregistry.EntryWithSchema) botregistry.Entry { return e.Entry }, opts.Categories, opts.Tags))
	case "tree":
		entries, diags, err := botregistry.ListWithSchemaDiagnostics(botregistry.ListOptions{Paths: opts.Paths})
		if err != nil {
			return err
		}
		warnDiscoveryErrors(opts.ErrW, diags)
		return renderBotsTree(w, filterByTaxonomy(entries, func(e botregistry.EntryWithSchema) botregistry.Entry { return e.Entry }, opts.Categories, opts.Tags))
	default:
		return fmt.Errorf("bots: unknown format %q (json|markdown|skill|tree)", opts.Format)
	}
}

// filterByTaxonomy applies the taxonomy filters to any entry list. One
// implementation over an accessor: BotEntry and botregistry.Entry are the
// same type (alias), EntryWithSchema embeds it — the accessor is a
// one-liner either way.
func filterByTaxonomy[T any](entries []T, entryOf func(T) botregistry.Entry, categories, tags []string) []T {
	categories = bundle.NormalizeBotTagList(categories) // trim + lowercase + dedup — the declared form
	tags = bundle.NormalizeBotTagList(tags)
	if len(categories) == 0 && len(tags) == 0 {
		return entries
	}
	out := make([]T, 0, len(entries))
	for _, e := range entries {
		if botMatchesTaxonomy(entryOf(e), categories, tags) {
			out = append(out, e)
		}
	}
	return out
}

// warnUnknownTaxonomyValues names filter values the vocabulary does not
// know, so a typo is distinguishable from "no such bots exist" — a
// silent empty result is a wrong-negative the operator cannot see.
func warnUnknownTaxonomyValues(w io.Writer, categories, tags []string) {
	if w == nil {
		return
	}
	for _, c := range bundle.NormalizeBotTagList(categories) {
		if c != "uncategorized" {
			if _, ok := bundle.BotCategoryBySlug(c); !ok {
				fmt.Fprintf(w, "bots: unknown category %q — known: %s (+ \"uncategorized\")\n", c, bundle.KnownBotCategorySlugs())
			}
		}
	}
	for _, t := range bundle.NormalizeBotTagList(tags) {
		if !bundle.IsKnownBotTag(t) {
			fmt.Fprintf(w, "bots: unseeded tag %q — matching anyway; known seed: %s\n", t, bundle.KnownBotTagsJoined())
		}
	}
}

// botMatchesTaxonomy reports whether one bot passes the filters: ANY
// requested category (the pseudo-slug "uncategorized" selects exactly the
// Uncategorized GROUP — no category or an unknown one — so the filter and
// the grouping agree) and EVERY requested tag.
func botMatchesTaxonomy(e botregistry.Entry, categories, tags []string) bool {
	if len(categories) > 0 && !slices.ContainsFunc(categories, func(c string) bool {
		if c == "uncategorized" {
			return bundle.IsUncategorizedSlug(e.Category)
		}
		return e.Category == c
	}) {
		return false
	}
	for _, t := range tags {
		if !slices.Contains(e.Tags, t) {
			return false
		}
	}
	return true
}

// warnDiscoveryErrors reports each skipped malformed source on w. Kept
// off the primary writer so the JSON/markdown payload stays parseable.
func warnDiscoveryErrors(w io.Writer, diags []botregistry.DiscoveryError) {
	if w == nil {
		return
	}
	for _, d := range diags {
		fmt.Fprintf(w, "bots: skipping %s: %s\n", d.Path, d.Error)
	}
}

// BotsRegenCatalog regenerates the orchestrator-facing bot catalog (the
// generated region of the whats-next bundle's iterion-bot-catalog.md)
// from the live manifests discovered under workdir, applying the
// workspace overlay. Returns the written path, or "" when the workspace
// ships no catalog template. The runtime regenerates this automatically
// at whats-next start and the studio on every bot-metadata save; this is
// the manual escape hatch (and the way to refresh the committed copy).
func BotsRegenCatalog(workdir string) (string, error) {
	return botregistry.RegenerateWhatsNextCatalog(workdir)
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

func renderBotsMarkdown(w io.Writer, entries []BotEntry) error {
	fmt.Fprintln(w, "# Bots")
	fmt.Fprintln(w)
	for _, group := range bundle.GroupByCategory(entries, func(e BotEntry) string { return e.Category }) {
		if len(group.Bots) == 0 {
			continue
		}
		fmt.Fprintf(w, "## %s — %s\n\n", group.Category.Title, group.Category.Tagline)
		for _, e := range group.Bots {
			if e.DisplayName != "" {
				fmt.Fprintf(w, "### %s · `%s`\n\n", e.DisplayName, e.Name)
			} else {
				fmt.Fprintf(w, "### `%s`\n\n", e.Name)
			}
			if e.Description != "" {
				fmt.Fprintf(w, "%s\n\n", e.Description)
			}
			fmt.Fprintf(w, "- Path: `%s`\n", e.Path)
			if len(e.Tags) > 0 {
				fmt.Fprintf(w, "- Tags: %s\n", strings.Join(e.Tags, ", "))
			}
			if len(e.Triggers) > 0 {
				fmt.Fprintf(w, "- Triggers: %s\n", strings.Join(e.Triggers, ", "))
			}
			if len(e.Capabilities) > 0 {
				fmt.Fprintf(w, "- Capabilities: %s\n", strings.Join(e.Capabilities, ", "))
			}
			fmt.Fprintln(w)
		}
	}
	return nil
}

// renderBotsTree emits the navigation spine: category sections (canonical
// order, Uncategorized last and always printed so the landmark exists),
// each bot indented under its category with its persona, and — the third
// level — the bot's presets as named leaves. This is the "what exists"
// view: one screen answering the whole shape of the fleet.
func renderBotsTree(w io.Writer, entries []botregistry.EntryWithSchema) error {
	for _, group := range bundle.GroupByCategory(entries, func(e botregistry.EntryWithSchema) string { return e.Category }) {
		fmt.Fprintf(w, "%s — %s (%d)\n", group.Category.Title, group.Category.Tagline, len(group.Bots))
		for _, e := range group.Bots {
			icon := strings.TrimSpace(e.Icon)
			label := e.DisplayName
			if strings.TrimSpace(label) == "" {
				label = e.Name
			}
			fmt.Fprintf(w, "  %s %s · %s\n", icon, label, e.Name)
			for _, p := range presetNames(e) {
				fmt.Fprintf(w, "      · %s\n", p)
			}
		}
		fmt.Fprintln(w)
	}
	return nil
}

// presetNames lists a bot's preset display names (falling back to the
// preset name), sorted for stable output.
func presetNames(e botregistry.EntryWithSchema) []string {
	if e.Presets == nil {
		return nil
	}
	names := make([]string, 0, len(e.Presets.Entries))
	for _, p := range e.Presets.Entries {
		if strings.TrimSpace(p.DisplayName) != "" {
			names = append(names, p.DisplayName)
		} else {
			names = append(names, p.Name)
		}
	}
	sort.Strings(names)
	return names
}

// renderBotsSkill emits a self-contained SKILL.md catalog: the standard
// front-matter, then the shared persona table + per-bot cards (the
// generated region of the live whats-next catalog), then the assignment
// heuristics. This is a standalone introspection view —
// botregistry.RegenerateWhatsNextCatalog produces the richer file Nexie
// actually reads by splicing the same block into a hand-authored
// decision-tree preamble.
func renderBotsSkill(w io.Writer, entries []botregistry.EntryWithSchema) error {
	fmt.Fprintln(w, "---")
	fmt.Fprintln(w, "name: iterion-bot-catalog")
	fmt.Fprintln(w, "description: |")
	fmt.Fprintln(w, "  Canonical list of bots available to dispatch via the iterion dispatcher.")
	fmt.Fprintln(w, "  Use this when deciding which bot to assign an issue to. Each card lists")
	fmt.Fprintln(w, "  the triggers, vars, and a when-to-use blurb so the matcher can pick by")
	fmt.Fprintln(w, "  intent.")
	fmt.Fprintln(w, "---")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "# iterion bot catalog")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Regenerate with `iterion bots list --format=skill`.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, botregistry.RenderCatalogBlock(entries, "", ""))
	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Assignment heuristics")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "1. Read the issue's title and labels.")
	fmt.Fprintln(w, "2. Match against each card's **Triggers** and **Use when**.")
	fmt.Fprintln(w, "3. If multiple bots match, pick the one whose description best fits the issue.")
	fmt.Fprintln(w, "4. If nothing matches cleanly, assign to a generalist (e.g. `feature_dev`) and add a `needs-triage` label.")
	return nil
}
