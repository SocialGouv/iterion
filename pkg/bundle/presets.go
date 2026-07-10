package bundle

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v2"
)

// PresetSpec is a file-based preset parsed from a bundle's
// presets/<name>.md (YAML frontmatter + markdown body). It is the on-disk
// authoring form of a "sous-bot": a named launch-time specialization that
// layers variable overrides, a system-prompt bias, and relevant skill
// hints onto an existing bot. The runtime converts it into an ir.Preset —
// the bundle package stays decoupled from pkg/dsl/ir.
type PresetSpec struct {
	// Name is the preset id, selected via `--preset <name>`. Defaults to
	// the file stem unless the frontmatter `name:` overrides it.
	Name string

	// DisplayName is the operator-facing label (e.g. "Improve Quality
	// (SRE)"). Optional; the studio falls back to Name.
	DisplayName string

	// Description is a one-line summary for the studio Launch picker.
	Description string

	// Vars are variable overrides applied to the run with precedence
	// defaults < preset < --var. Values are YAML-native (string / bool /
	// int / float); the engine coerces each to the declared var's type and
	// silently drops keys the workflow doesn't declare, exactly like a
	// stray --var.
	Vars map[string]any

	// Skills lists bundle skill names this preset makes relevant (e.g.
	// "lang-js-fallow"). Every bundle skill is mirrored into the workspace
	// regardless; this list is surfaced as a hint in the run-time "## Focus"
	// prompt section and in the studio.
	Skills []string

	// Prompt is the markdown body: the bias appended to every LLM node's
	// system prompt at run time. Supports `{{vars.X}}` template refs,
	// resolved per node. Empty for a var-only preset.
	Prompt string
}

// presetFrontmatter is the strict YAML shape of a preset's frontmatter.
// Unknown keys are rejected (UnmarshalStrict) so a typo surfaces as a
// clear error rather than being silently ignored.
type presetFrontmatter struct {
	Name        string         `yaml:"name"`
	DisplayName string         `yaml:"display_name"`
	Description string         `yaml:"description"`
	Vars        map[string]any `yaml:"vars"`
	Skills      []string       `yaml:"skills"`
}

// LoadPresets reads every presets/<name>.md file under dir and returns the
// parsed presets sorted by name. dir is a bundle's PresetsDir; an empty or
// missing dir returns nil without error. A single file that fails to parse
// is skipped and its error collected in the second return value, so one
// malformed preset never blocks the rest of the bundle's presets.
func LoadPresets(dir string) ([]PresetSpec, []error) {
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []error{fmt.Errorf("bundle: read presets dir %s: %w", dir, err)}
	}
	var (
		out  []PresetSpec
		errs []error
		seen = map[string]string{} // name -> file, to flag collisions
	)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		ps, perr := loadPresetFile(path)
		if perr != nil {
			errs = append(errs, perr)
			continue
		}
		if prev, dup := seen[ps.Name]; dup {
			errs = append(errs, fmt.Errorf("bundle: duplicate preset name %q (%s and %s)", ps.Name, prev, path))
			continue
		}
		seen[ps.Name] = path
		out = append(out, ps)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, errs
}

// loadPresetFile parses one presets/<name>.md file.
func loadPresetFile(path string) (PresetSpec, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return PresetSpec{}, fmt.Errorf("bundle: read preset %s: %w", path, err)
	}
	fmBytes, mdBody := splitFrontmatter(body)
	var fm presetFrontmatter
	if len(strings.TrimSpace(string(fmBytes))) > 0 {
		if err := yaml.UnmarshalStrict(fmBytes, &fm); err != nil {
			return PresetSpec{}, fmt.Errorf("bundle: parse preset %s frontmatter: %w", path, err)
		}
	}
	name := fm.Name
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	if err := validatePresetName(name); err != nil {
		return PresetSpec{}, fmt.Errorf("bundle: preset %s: %w", path, err)
	}
	return PresetSpec{
		Name:        name,
		DisplayName: fm.DisplayName,
		Description: fm.Description,
		Vars:        fm.Vars,
		Skills:      fm.Skills,
		Prompt:      strings.TrimSpace(string(mdBody)),
	}, nil
}

// validatePresetName rejects empty or path-like preset names. The name is
// used as a map key and a `--preset` argument, never joined to a path, but
// guarding against separators keeps it unambiguous and future-proof.
func validatePresetName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("preset name is empty")
	}
	if name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("invalid preset name %q (no path separators)", name)
	}
	return nil
}

// splitFrontmatter separates a leading YAML frontmatter block (delimited by
// lines containing only "---") from the markdown body. When no frontmatter
// is present the whole input is returned as the body. The opening "---"
// must be the first non-empty line (leading blank lines tolerated),
// matching the SKILL.md / manifest convention. An unterminated frontmatter
// block treats everything after the opener as frontmatter with no body.
func splitFrontmatter(data []byte) (frontmatter, body []byte) {
	lines := strings.Split(string(data), "\n")
	open := 0
	for open < len(lines) && strings.TrimSpace(lines[open]) == "" {
		open++
	}
	if open >= len(lines) || strings.TrimSpace(lines[open]) != "---" {
		return nil, data
	}
	for j := open + 1; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) == "---" {
			fm := strings.Join(lines[open+1:j], "\n")
			md := strings.Join(lines[j+1:], "\n")
			return []byte(fm), []byte(md)
		}
	}
	return []byte(strings.Join(lines[open+1:], "\n")), nil
}
