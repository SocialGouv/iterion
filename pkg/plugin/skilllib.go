package plugin

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v2"
)

// SynthesizeSkillsManifest builds a skills-only Manifest for a directory that
// ships bare Claude-style skills but no plugin.yaml — the common shape of a
// public skill library. It collects markdown skills under <dir>/skills/
// (recursively); if there is no skills/ directory it falls back to top-level
// *.md files. The returned manifest is disabled by default (an installed
// third-party skill pack should be opt-in). Returns an error when no skill
// files are found, so the caller can report that the repo is not installable.
func SynthesizeSkillsManifest(name, dir string) (*Manifest, error) {
	rels, err := collectSkillFiles(dir)
	if err != nil {
		return nil, err
	}
	if len(rels) == 0 {
		return nil, fmt.Errorf("plugin: %q has no plugin.yaml and no skills/ or top-level *.md to install as a skill library", dir)
	}
	return &Manifest{
		Name:          NormalizeName(name),
		Version:       "0.0.0+synthesized",
		Description:   fmt.Sprintf("Skill library (%d skill(s)) — synthesized on install", len(rels)),
		SchemaVersion: SchemaVersion,
		// Installed skill packs are opt-in: the operator enables them explicitly.
		DefaultEnabled: false,
		Contributes:    Contributes{Skills: rels},
	}, nil
}

// collectSkillFiles returns skill markdown paths relative to dir (slash-form,
// sorted). Prefers <dir>/skills/**.md; falls back to top-level *.md.
func collectSkillFiles(dir string) ([]string, error) {
	var rels []string
	skillsDir := filepath.Join(dir, "skills")
	if fi, err := os.Stat(skillsDir); err == nil && fi.IsDir() {
		walkErr := filepath.WalkDir(skillsDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.EqualFold(filepath.Ext(path), ".md") {
				return nil
			}
			rel, rerr := filepath.Rel(dir, path)
			if rerr != nil {
				return rerr
			}
			rels = append(rels, filepath.ToSlash(rel))
			return nil
		})
		if walkErr != nil {
			return nil, fmt.Errorf("plugin: scan skills in %q: %w", dir, walkErr)
		}
		sort.Strings(rels)
		return rels, nil
	}
	// Fallback: top-level *.md (e.g. a single SKILL.md at the repo root).
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".md") &&
			!strings.EqualFold(e.Name(), "README.md") {
			rels = append(rels, e.Name())
		}
	}
	sort.Strings(rels)
	return rels, nil
}

// NormalizeName derives a kebab-case plugin name from a directory path or git
// URL: it takes the last path segment, strips a trailing ".git", lowercases,
// and replaces any run of non-alphanumeric characters with a single dash.
func NormalizeName(src string) string {
	s := strings.TrimSpace(src)
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	s = strings.ToLower(s)
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "skill-library"
	}
	return out
}

// WriteManifest marshals m to <dir>/plugin.yaml. Used by the install path to
// persist a synthesized manifest into an installed skill library.
func WriteManifest(dir string, m *Manifest) error {
	data, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("plugin: marshal synthesized manifest: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, ManifestFile), data, 0o644)
}

// LoadDir reads a plugin from an arbitrary directory — a tree fetched from a
// git remote rather than installed under the iterion home.
//
// It accepts both shapes an operator can host in a repository: a full
// plugin.yaml, or a bare skills/ library (no manifest), for which it
// synthesizes a skills-only manifest exactly like `iterion plugin install`
// does. name seeds the synthesized manifest and is ignored when the directory
// carries its own manifest.
//
// Enabled is left to the caller: a PluginSource carries its own enable state,
// which is the authority for a git-hosted plugin (default_enabled belongs to
// the artifact, not to the operator's binding).
func LoadDir(name, dir string) (*Plugin, error) {
	var m *Manifest
	data, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	switch {
	case err == nil:
		if m, err = ParseManifest(data); err != nil {
			return nil, fmt.Errorf("plugin: parse %s in %s: %w", ManifestFile, dir, err)
		}
	case os.IsNotExist(err):
		if m, err = SynthesizeSkillsManifest(name, dir); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("plugin: read %s in %s: %w", ManifestFile, dir, err)
	}
	return &Plugin{Manifest: *m, Dir: dir, fsys: os.DirFS(dir)}, nil
}
