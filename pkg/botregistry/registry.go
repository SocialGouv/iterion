// Package botregistry discovers bots on disk: single .bot/.iter files
// and .botz bundles. It is the shared layer used by the `iterion bots
// list` CLI command, the studio HTTP server (GET /api/v1/bots), and the
// dispatcher when resolving a per-ticket bot override to a workflow
// file path.
//
// Discovery is purely metadata (name, description, triggers,
// capabilities). The companion file schema.go layers on the workflow's
// declared vars + presets so the studio can render a typed form per
// bot.
package botregistry

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v2"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// Entry is one bot discovered by List. Path is the file (single .bot)
// or directory (bundle) that produced the entry — operators can grep
// back to it.
type Entry struct {
	Name         string   `json:"name" yaml:"name"`
	Description  string   `json:"description" yaml:"description,omitempty"`
	Path         string   `json:"path" yaml:"path"`
	Triggers     []string `json:"triggers,omitempty" yaml:"triggers,omitempty"`
	Capabilities []string `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
}

// IsBundle reports whether the entry came from a .botz bundle (Path
// points at a directory containing manifest.yaml + main.bot) rather
// than a single .bot/.iter file.
func (e Entry) IsBundle() bool {
	info, err := os.Stat(e.Path)
	return err == nil && info.IsDir()
}

// MainFile returns the workflow source file the entry points at. For
// a bundle this is <Path>/main.bot; for a loose file it is Path itself.
// Used by the dispatcher to resolve a per-ticket bot override to a
// concrete workflow path.
func (e Entry) MainFile() string {
	if e.IsBundle() {
		return filepath.Join(e.Path, "main.bot")
	}
	return e.Path
}

// ListOptions configures discovery for List.
type ListOptions struct {
	// Paths are the roots to walk. Each may be a single .bot file
	// (treated as one entry), a .botz bundle directory, or a directory
	// containing many .bot files / sub-bundles. Missing paths are
	// skipped silently — the caller's defaults often include
	// optimistic locations like "./examples" or "./bots".
	Paths []string
}

// Config carries the discovery roots for the bot registry. Lives here
// so both pkg/server (studio HTTP endpoint) and pkg/dispatcher
// (per-ticket bot override resolution) can declare the same field
// without one importing the other.
type Config struct {
	Paths []string `yaml:"paths,omitempty" json:"paths,omitempty"`
}

// List walks Opts.Paths and returns the discovered bots sorted by
// name. A missing path is treated as empty (not an error) so callers
// can pass optimistic defaults.
func List(opts ListOptions) ([]Entry, error) {
	entries, err := discoverBots(opts.Paths)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

// ResolveBotPath looks up a bot by name across paths and returns the
// path to its workflow source file (bundle's main.bot or loose .bot).
// Returns os.ErrNotExist when no bot with that name is found.
func ResolveBotPath(name string, paths []string) (string, error) {
	entries, err := List(ListOptions{Paths: paths})
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.Name == name {
			return e.MainFile(), nil
		}
	}
	return "", fmt.Errorf("bot %q not found in %v: %w", name, paths, os.ErrNotExist)
}

// discoverBots walks each root and produces one Entry per discovered
// bot. Bundles (directories with manifest.yaml + main.bot) collapse
// into one entry; individual .bot/.iter files become one entry each.
// Missing roots are skipped silently so callers can pass optimistic
// default paths.
func discoverBots(roots []string) ([]Entry, error) {
	var entries []Entry
	seen := map[string]bool{}

	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("bots: stat %s: %w", root, err)
		}
		if !info.IsDir() {
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
					return filepath.SkipDir
				}
				return nil
			}
			name := d.Name()
			if !strings.HasSuffix(name, ".bot") && !strings.HasSuffix(name, ".iter") {
				return nil
			}
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

func parseBundle(dir string) (*Entry, error) {
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
	if fm := readFrontmatter(filepath.Join(dir, "main.bot")); fm != nil {
		if len(fm.Triggers) > 0 {
			m.Triggers = fm.Triggers
		}
		if len(fm.Capabilities) > 0 {
			m.Capabilities = fm.Capabilities
		}
	}
	return &Entry{
		Name:         m.Name,
		Description:  strings.TrimSpace(m.Description),
		Path:         dir,
		Triggers:     m.Triggers,
		Capabilities: m.Capabilities,
	}, nil
}

func parseBotFile(path string) (*Entry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("bots: read %s: %w", path, err)
	}
	fm := parseFrontmatterBody(raw)
	e := &Entry{Path: path}
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

func readFrontmatter(path string) *frontmatter {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return parseFrontmatterBody(raw)
}

// parseFrontmatterBody pulls a `## ---` … `## ---` block from the top
// of the file and YAML-decodes the inner content. The block is allowed
// only at the very top of the file, optionally after blank lines.
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

// leadingCommentDescription returns the first paragraph of `## ` lines
// at the top of the file (excluding any `## ---` framing). Stops at the
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
