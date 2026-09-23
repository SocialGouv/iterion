package repomap

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/internal/treeskip"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
	"github.com/SocialGouv/iterion/pkg/skilllib"
)

// botBundles maps the catalog: one row per bundle, and one per skill.
//
// The skill rows are the cheap half of progressive disclosure written
// down — a skill already exposes only `name` and `description` until the
// task matches it, and this file is that same pair, collected, so an
// author can see what already exists before writing a fourth variant of
// it. The frontmatter is read with the engine's own parser
// (skilllib.ScanFrontmatter), not a second one that would drift.
type botBundles struct{}

func (botBundles) Stem() string  { return "bots" }
func (botBundles) Title() string { return "Bot and skill map" }

type botRow struct {
	Name        string
	Display     string
	Icon        string
	Version     string
	Description string
	Skills      int
	Workflows   int
}

type skillRow struct {
	Bot         string
	File        string
	Name        string
	Description string
}

func (b botBundles) Extract(root string) (string, error) {
	botsDir := filepath.Join(root, "bots")
	entries, err := os.ReadDir(botsDir)
	if err != nil {
		return "", fmt.Errorf("read bots/: %w", err)
	}

	var bots []botRow
	var skills []skillRow
	for _, e := range entries {
		if !e.IsDir() || treeskip.Dir(e.Name()) {
			continue
		}
		// A bundle is a directory with a manifest. Without this test the
		// map called `bots/testdata/` — a fixture tree — a shipped bot,
		// and published "38 bundles" where the catalog has 36.
		if _, err := os.Stat(filepath.Join(botsDir, e.Name(), "manifest.yaml")); err != nil {
			continue
		}
		row, botSkills, err := readBundle(botsDir, e.Name())
		if err != nil {
			return "", err
		}
		bots = append(bots, row)
		skills = append(skills, botSkills...)
	}
	sort.Slice(bots, func(i, j int) bool { return bots[i].Name < bots[j].Name })
	sort.Slice(skills, func(i, j int) bool {
		if skills[i].Bot != skills[j].Bot {
			return skills[i].Bot < skills[j].Bot
		}
		return skills[i].File < skills[j].File
	})

	var sb strings.Builder
	fmt.Fprintf(&sb, "%d bundles under `bots/`, carrying %d skills between them.\n\n",
		len(bots), len(skills))
	sb.WriteString("## Bundles\n\n")
	sb.WriteString("| Bot | Persona | What it does | `.bot` files · skills | Version |\n|---|---|---|---|---|\n")
	for _, r := range bots {
		persona := strings.TrimSpace(r.Icon + " " + r.Display)
		fmt.Fprintf(&sb, "| `%s` | %s | %s | %d · %d | %s |\n",
			r.Name, orDash(persona), orDash(r.Description), r.Workflows, r.Skills, orDash(r.Version))
	}

	sb.WriteString("\n## Skills\n\n")
	sb.WriteString("The pair a model sees before it decides to open the file.\n\n")
	sb.WriteString("| Bot | Skill | Description |\n|---|---|---|\n")
	for _, s := range skills {
		fmt.Fprintf(&sb, "| `%s` | `%s` | %s |\n", s.Bot, orDash(s.Name), orDash(s.Description))
	}
	return sb.String(), nil
}

func readBundle(botsDir, name string) (botRow, []skillRow, error) {
	dir := filepath.Join(botsDir, name)
	row := botRow{Name: name}

	// A manifest the loader REFUSES — invalid YAML, an unknown schema
	// version, an attachment that escapes the bundle — is an error, not
	// an empty row. Swallowing it let `task map:gen` overwrite the map
	// with a blank bundle and turn the gate green: the gate's own
	// remediation instruction became the laundering path.
	m, err := bundle.LoadManifest(filepath.Join(dir, "manifest.yaml"))
	if err != nil {
		return row, nil, fmt.Errorf("bots/%s/manifest.yaml: %w", name, err)
	}
	if m != nil {
		row.Display = m.DisplayName
		row.Icon = m.Icon
		row.Version = m.Version
		row.Description = firstSentence(m.Description, 140)
		if m.Name != "" {
			row.Name = m.Name
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return row, nil, fmt.Errorf("read bots/%s: %w", name, err)
	}
	for _, e := range entries {
		// workflowfile owns the accepted extension (CLAUDE.md names it
		// the single source of truth); a second spelling here would
		// drift the day a second extension lands.
		if !e.IsDir() && workflowfile.IsWorkflowFile(e.Name()) {
			row.Workflows++
		}
	}

	skillsDir := filepath.Join(dir, "skills")
	skillEntries, err := os.ReadDir(skillsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return row, nil, nil // a bundle without skills is ordinary
	}
	if err != nil {
		// Anything else — a file where a directory belongs, a permission
		// refusal — is a hole in the map, and a map that hides its holes
		// lies by their size.
		return row, nil, fmt.Errorf("bots/%s/skills: %w", name, err)
	}
	var skills []skillRow
	for _, e := range skillEntries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		f, err := os.Open(filepath.Join(skillsDir, e.Name()))
		if err != nil {
			return row, nil, fmt.Errorf("open bots/%s/skills/%s: %w", name, e.Name(), err)
		}
		skillName, desc := skilllib.ScanFrontmatter(f)
		_ = f.Close()
		skills = append(skills, skillRow{
			Bot:         row.Name,
			File:        e.Name(),
			Name:        skillName,
			Description: firstSentence(desc, 130),
		})
	}
	row.Skills = len(skills)
	return row, skills, nil
}
