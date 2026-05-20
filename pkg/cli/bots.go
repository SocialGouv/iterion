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
}

// BotsList walks Opts.Paths, parses metadata, and writes the result to w.
func BotsList(opts BotsListOptions, w io.Writer) error {
	if len(opts.Paths) == 0 {
		return fmt.Errorf("bots: no paths specified")
	}
	if opts.Format == "" {
		opts.Format = "json"
	}
	entries, err := botregistry.List(botregistry.ListOptions{Paths: opts.Paths})
	if err != nil {
		return err
	}

	switch opts.Format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	case "markdown":
		return renderBotsMarkdown(w, entries)
	case "skill":
		return renderBotsSkill(w, entries)
	default:
		return fmt.Errorf("bots: unknown format %q (json|markdown|skill)", opts.Format)
	}
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

func renderBotsMarkdown(w io.Writer, entries []BotEntry) error {
	fmt.Fprintln(w, "# Bots")
	fmt.Fprintln(w)
	for _, e := range entries {
		fmt.Fprintf(w, "## %s\n\n", e.Name)
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

// renderBotsSkill emits a SKILL.md ready to drop into a bundle's skills/
// directory. The output is a decision-tree-style catalog the LLM can
// consult to pick a bot for a given issue.
func renderBotsSkill(w io.Writer, entries []BotEntry) error {
	fmt.Fprintln(w, "---")
	fmt.Fprintln(w, "name: iterion-bot-catalog")
	fmt.Fprintln(w, "description: |")
	fmt.Fprintln(w, "  Canonical list of bots available to dispatch via the iterion dispatcher.")
	fmt.Fprintln(w, "  Use this when deciding which bot to assign an issue to. Each entry lists")
	fmt.Fprintln(w, "  the triggers it expects, the capabilities it consumes, and a one-line")
	fmt.Fprintln(w, "  description so the matcher can pick by intent.")
	fmt.Fprintln(w, "---")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "# iterion bot catalog")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Regenerate with `iterion bots list --format=skill --paths examples/`.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| Bot | Description | Triggers | Capabilities |")
	fmt.Fprintln(w, "|---|---|---|---|")
	for _, e := range entries {
		desc := strings.ReplaceAll(strings.TrimSpace(e.Description), "\n", " ")
		if len(desc) > 200 {
			desc = desc[:197] + "..."
		}
		fmt.Fprintf(w, "| `%s` | %s | %s | %s |\n",
			e.Name,
			desc,
			joinOrDash(e.Triggers),
			joinOrDash(e.Capabilities),
		)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Assignment heuristics")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "1. Read the issue's title and labels.")
	fmt.Fprintln(w, "2. Match against the **Triggers** column above.")
	fmt.Fprintln(w, "3. If multiple bots match, pick the one whose **Description** best fits the issue.")
	fmt.Fprintln(w, "4. If nothing matches cleanly, assign to a generalist (e.g. `feature_dev`) and add a `needs-triage` label.")
	return nil
}

func joinOrDash(xs []string) string {
	if len(xs) == 0 {
		return "—"
	}
	return strings.Join(xs, ", ")
}
