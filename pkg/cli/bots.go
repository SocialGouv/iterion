package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/SocialGouv/iterion/pkg/botregistry"
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
	// or "skill" (a SKILL.md ready to drop in a `<bundle>/skills/`).
	Format string

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
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	case "markdown":
		entries, diags, err := botregistry.ListWithDiagnostics(botregistry.ListOptions{Paths: opts.Paths})
		if err != nil {
			return err
		}
		warnDiscoveryErrors(opts.ErrW, diags)
		return renderBotsMarkdown(w, entries)
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
		return renderBotsSkill(w, entries)
	default:
		return fmt.Errorf("bots: unknown format %q (json|markdown|skill)", opts.Format)
	}
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
	for _, e := range entries {
		if e.DisplayName != "" {
			fmt.Fprintf(w, "## %s · `%s`\n\n", e.DisplayName, e.Name)
		} else {
			fmt.Fprintf(w, "## %s\n\n", e.Name)
		}
		if e.Description != "" {
			fmt.Fprintf(w, "%s\n\n", e.Description)
		}
		fmt.Fprintf(w, "- Path: `%s`\n", e.Path)
		if len(e.Triggers) > 0 {
			fmt.Fprintf(w, "- Triggers: %s\n", strings.Join(e.Triggers, ", "))
		}
		if len(e.Capabilities) > 0 {
			fmt.Fprintf(w, "- Capabilities: %s\n", strings.Join(e.Capabilities, ", "))
		}
		fmt.Fprintln(w)
	}
	return nil
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
