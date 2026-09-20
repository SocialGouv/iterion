package cli

import (
	"encoding/json"
	"fmt"
	"io"
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

	switch opts.Format {
	case "json":
		entries, diags, err := botregistry.ListWithDiagnostics(botregistry.ListOptions{Paths: opts.Paths})
		if err != nil {
			return err
		}
		warnDiscoveryErrors(opts.ErrW, diags)
		entries = filterBots(entries, opts.Categories, opts.Tags)
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	case "markdown":
		entries, diags, err := botregistry.ListWithDiagnostics(botregistry.ListOptions{Paths: opts.Paths})
		if err != nil {
			return err
		}
		warnDiscoveryErrors(opts.ErrW, diags)
		return renderBotsMarkdown(w, filterBots(entries, opts.Categories, opts.Tags))
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
		return renderBotsSkill(w, filterSchemaBots(entries, opts.Categories, opts.Tags))
	case "tree":
		entries, diags, err := botregistry.ListWithSchemaDiagnostics(botregistry.ListOptions{Paths: opts.Paths})
		if err != nil {
			return err
		}
		warnDiscoveryErrors(opts.ErrW, diags)
		return renderBotsTree(w, filterSchemaBots(entries, opts.Categories, opts.Tags))
	default:
		return fmt.Errorf("bots: unknown format %q (json|markdown|skill|tree)", opts.Format)
	}
}

// normalizedTaxonomyFilters form-normalizes the CLI's --category/--tag
// values the same way the manifest loader normalizes declarations, so
// `--category Verify` matches `category: verify`.
func normalizedTaxonomyFilters(values []string) []string {
	var out []string
	for _, v := range values {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// filterBots applies the taxonomy filters to a plain entry list.
func filterBots(entries []BotEntry, categories, tags []string) []BotEntry {
	categories = normalizedTaxonomyFilters(categories)
	tags = normalizedTaxonomyFilters(tags)
	if len(categories) == 0 && len(tags) == 0 {
		return entries
	}
	out := make([]BotEntry, 0, len(entries))
	for _, e := range entries {
		if botMatchesTaxonomy(e, categories, tags) {
			out = append(out, e)
		}
	}
	return out
}

// filterSchemaBots is filterBots over the schema-augmented list.
func filterSchemaBots(entries []botregistry.EntryWithSchema, categories, tags []string) []botregistry.EntryWithSchema {
	categories = normalizedTaxonomyFilters(categories)
	tags = normalizedTaxonomyFilters(tags)
	if len(categories) == 0 && len(tags) == 0 {
		return entries
	}
	out := make([]botregistry.EntryWithSchema, 0, len(entries))
	for _, e := range entries {
		if botMatchesTaxonomy(e.Entry, categories, tags) {
			out = append(out, e)
		}
	}
	return out
}

// botMatchesTaxonomy reports whether one bot passes the filters: any
// requested category (the pseudo-slug "uncategorized" matches an empty
// declared category) and every requested tag.
func botMatchesTaxonomy(e botregistry.Entry, categories, tags []string) bool {
	for _, c := range categories {
		if c == "uncategorized" {
			if e.Category == "" {
				continue
			}
			return false
		}
		if e.Category == c {
			continue
		}
		return false
	}
	for _, t := range tags {
		found := false
		for _, bt := range e.Tags {
			if bt == t {
				found = true
				break
			}
		}
		if !found {
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
	for _, group := range groupBotsForDisplay(entries) {
		if len(group.bots) == 0 {
			continue
		}
		fmt.Fprintf(w, "## %s — %s\n\n", group.title, group.tagline)
		for _, e := range group.bots {
			if e.DisplayName != "" {
				fmt.Fprintf(w, "### %s · `%s`\n\n", e.DisplayName, e.Name)
			} else {
				fmt.Fprintf(w, "### `%s`\n\n", e.Name)
			}
			if e.Description != "" {
				fmt.Fprintf(w, "%s\n\n", e.Description)
			}
			fmt.Fprintf(w, "- Path: `%s`\n", e.Path)
			if e.Category != "" {
				fmt.Fprintf(w, "- Category: %s\n", e.Category)
			}
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
	for _, group := range groupSchemaBotsForDisplay(entries) {
		fmt.Fprintf(w, "%s — %s (%d)\n", group.title, group.tagline, len(group.bots))
		for _, e := range group.bots {
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

// botDisplayGroup is one category section shared by the tree and markdown
// renderers.
type botDisplayGroup struct {
	title   string
	tagline string
	bots    []BotEntry
}

// groupBotsForDisplay buckets bots into canonical category order with the
// Uncategorized group last. Groups are returned populated or not — the
// renderer decides whether an empty category is a landmark worth a line.
func groupBotsForDisplay(entries []BotEntry) []botDisplayGroup {
	groups := make([]botDisplayGroup, 0, len(bundle.BotCategories)+1)
	for _, c := range bundle.BotCategories {
		groups = append(groups, botDisplayGroup{title: c.Title, tagline: c.Tagline})
	}
	groups = append(groups, botDisplayGroup{title: "Uncategorized", tagline: "visible, never hidden"})
	for _, e := range entries {
		placeBySlug(groups, e)
	}
	return groups
}

// placeBySlug appends e to the group whose slug matches e.Category, or to
// the trailing Uncategorized group.
func placeBySlug(groups []botDisplayGroup, e BotEntry) {
	for i := range bundle.BotCategories {
		if e.Category == bundle.BotCategories[i].Slug {
			groups[i].bots = append(groups[i].bots, e)
			return
		}
	}
	last := len(groups) - 1
	groups[last].bots = append(groups[last].bots, e)
}

// schemaDisplayGroup is botDisplayGroup over the schema-augmented entries
// (the tree needs presets).
type schemaDisplayGroup struct {
	title   string
	tagline string
	bots    []botregistry.EntryWithSchema
}

// groupSchemaBotsForDisplay is groupBotsForDisplay over EntryWithSchema.
func groupSchemaBotsForDisplay(entries []botregistry.EntryWithSchema) []schemaDisplayGroup {
	groups := make([]schemaDisplayGroup, 0, len(bundle.BotCategories)+1)
	for _, c := range bundle.BotCategories {
		groups = append(groups, schemaDisplayGroup{title: c.Title, tagline: c.Tagline})
	}
	groups = append(groups, schemaDisplayGroup{title: "Uncategorized", tagline: "visible, never hidden"})
	for _, e := range entries {
		placed := false
		for i := range bundle.BotCategories {
			if e.Category == bundle.BotCategories[i].Slug {
				groups[i].bots = append(groups[i].bots, e)
				placed = true
				break
			}
		}
		if !placed {
			last := len(groups) - 1
			groups[last].bots = append(groups[last].bots, e)
		}
	}
	return groups
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
