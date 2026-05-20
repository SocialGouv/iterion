package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v2"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// BotEntry is one bot discovered by [BotsList]. Source carries the file
// path the entry was derived from so the operator can grep back to it.
type BotEntry struct {
	Name         string   `json:"name" yaml:"name"`
	Description  string   `json:"description" yaml:"description,omitempty"`
	Path         string   `json:"path" yaml:"path"`
	Triggers     []string `json:"triggers,omitempty" yaml:"triggers,omitempty"`
	Capabilities []string `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
}

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
	entries, err := discoverBots(opts.Paths)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })

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

// discoverBots walks each root and produces one BotEntry per discovered
// bot. Bundles (directories with manifest.yaml + main.bot) collapse into
// one entry; individual .bot files become one entry each.
func discoverBots(roots []string) ([]BotEntry, error) {
	var entries []BotEntry
	seen := map[string]bool{}

	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil {
			return nil, fmt.Errorf("bots: stat %s: %w", root, err)
		}
		if !info.IsDir() {
			// Single-file path — must be a .bot.
			e, err := parseBotFile(root)
			if err != nil {
				return nil, err
			}
			if e != nil && !seen[e.Path] {
				entries = append(entries, *e)
				seen[e.Path] = true
			}
			continue
		}
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				// Bundle directory? Has manifest.yaml + main.bot.
				manifest := filepath.Join(path, "manifest.yaml")
				mainBot := filepath.Join(path, "main.bot")
				if fileExists(manifest) && fileExists(mainBot) {
					e, err := parseBundle(path)
					if err != nil {
						return err
					}
					if e != nil && !seen[e.Path] {
						entries = append(entries, *e)
						seen[e.Path] = true
					}
					return filepath.SkipDir // don't descend into a bundle
				}
				return nil
			}
			name := d.Name()
			if !strings.HasSuffix(name, ".bot") && !strings.HasSuffix(name, ".iter") {
				return nil
			}
			// Skip a main.bot inside a directory that wasn't recognised as a
			// bundle (no manifest) — treat it as a loose bot file under its
			// own name based on the parent dir.
			e, err := parseBotFile(path)
			if err != nil {
				return err
			}
			if e != nil && !seen[e.Path] {
				entries = append(entries, *e)
				seen[e.Path] = true
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return entries, nil
}

// parseBundle reads bundle/manifest.yaml + (optional) front-matter from
// bundle/main.bot to produce a single entry whose Path points at the
// bundle directory. Schema-version validation comes from
// [bundle.LoadManifest] so the catalog rejects bundles built for a
// future iterion.
func parseBundle(dir string) (*BotEntry, error) {
	m, err := bundle.LoadManifest(filepath.Join(dir, "manifest.yaml"))
	if err != nil {
		return nil, fmt.Errorf("bots: %w", err)
	}
	if m == nil {
		m = &bundle.Manifest{}
	}
	if m.Name == "" {
		m.Name = filepath.Base(dir)
	}
	// Fold frontmatter from main.bot if present (richer triggers/capabilities).
	if fm := readFrontmatter(filepath.Join(dir, "main.bot")); fm != nil {
		if len(fm.Triggers) > 0 {
			m.Triggers = fm.Triggers
		}
		if len(fm.Capabilities) > 0 {
			m.Capabilities = fm.Capabilities
		}
	}
	return &BotEntry{
		Name:         m.Name,
		Description:  strings.TrimSpace(m.Description),
		Path:         dir,
		Triggers:     m.Triggers,
		Capabilities: m.Capabilities,
	}, nil
}

// parseBotFile reads a single .bot/.iter file and pulls frontmatter from
// it. When no frontmatter is present, falls back to filename basename and
// the leading `##` comment block as description (if any).
func parseBotFile(path string) (*BotEntry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("bots: read %s: %w", path, err)
	}
	fm := parseFrontmatterBody(raw)
	e := &BotEntry{Path: path}
	if fm != nil {
		e.Name = fm.Name
		e.Description = fm.Description
		e.Triggers = fm.Triggers
		e.Capabilities = fm.Capabilities
	}
	if e.Name == "" {
		base := filepath.Base(path)
		e.Name = strings.TrimSuffix(strings.TrimSuffix(base, ".bot"), ".iter")
	}
	if e.Description == "" {
		e.Description = leadingCommentDescription(raw)
	}
	return e, nil
}

type frontmatter struct {
	Name         string   `yaml:"name"`
	Description  string   `yaml:"description"`
	Triggers     []string `yaml:"triggers"`
	Capabilities []string `yaml:"capabilities"`
}

// readFrontmatter wraps parseFrontmatterBody with a file read; returns
// nil when the file is missing or has no frontmatter block.
func readFrontmatter(path string) *frontmatter {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return parseFrontmatterBody(raw)
}

// parseFrontmatterBody pulls a `## ---` … `## ---` block (lines prefixed
// with `## `) from the top of the file and YAML-decodes the inner
// content. Returns nil when no block is present.
//
// The block is allowed only at the very top of the file, optionally
// after a shebang or blank lines.
func parseFrontmatterBody(raw []byte) *frontmatter {
	lines := strings.Split(string(raw), "\n")
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i >= len(lines) || strings.TrimSpace(lines[i]) != "## ---" {
		return nil
	}
	start := i + 1
	end := -1
	for j := start; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) == "## ---" {
			end = j
			break
		}
	}
	if end < 0 {
		return nil
	}
	var yamlLines []string
	for _, ln := range lines[start:end] {
		stripped := strings.TrimPrefix(ln, "## ")
		stripped = strings.TrimPrefix(stripped, "##")
		yamlLines = append(yamlLines, stripped)
	}
	var fm frontmatter
	if err := yaml.Unmarshal([]byte(strings.Join(yamlLines, "\n")), &fm); err != nil {
		return nil
	}
	return &fm
}

// leadingCommentDescription returns the first paragraph of `## ` lines at
// the top of the file (excluding any `## ---` framing). Stops at the
// first blank line or non-comment line.
func leadingCommentDescription(raw []byte) string {
	lines := strings.Split(string(raw), "\n")
	var out []string
	skippingFM := false
	for _, ln := range lines {
		trim := strings.TrimSpace(ln)
		if trim == "## ---" {
			skippingFM = !skippingFM
			continue
		}
		if skippingFM {
			continue
		}
		if !strings.HasPrefix(trim, "##") {
			if len(out) > 0 {
				break
			}
			continue
		}
		body := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(trim, "##"), " "))
		if body == "" {
			if len(out) > 0 {
				break
			}
			continue
		}
		out = append(out, body)
	}
	return strings.Join(out, " ")
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
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
