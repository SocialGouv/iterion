package tools

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SocialGouv/claw-code-go/internal/api"
	"gopkg.in/yaml.v3"
)

// SkillFrontmatter mirrors the YAML header used by Claude Code skills.
// Fields not present in the YAML default to their zero value.
type SkillFrontmatter struct {
	Name                   string   `yaml:"name"`
	Description            string   `yaml:"description"`
	ArgumentHint           string   `yaml:"argument-hint"`
	DisableModelInvocation bool     `yaml:"disable-model-invocation"`
	AllowedTools           []string `yaml:"allowed-tools"`
	Version                string   `yaml:"version"`
	License                string   `yaml:"license"`
}

const embedScheme = "embed://"

// SkillSource identifies where a resolved skill lives.
type SkillSource string

const (
	SkillSourceFilesystem SkillSource = "filesystem"
	SkillSourceBundled    SkillSource = "bundled"
)

// SkillResolution describes a successfully resolved skill on the filesystem
// or inside the embedded bundle.
type SkillResolution struct {
	// CanonicalName is "<plugin>:<skill>" when the skill lives under a
	// plugin namespace, or the bare skill name for legacy flat layouts.
	CanonicalName string
	// Path is either an absolute filesystem path or an embed:// URI.
	Path string
	// Source identifies whether the skill came from disk or the embed bundle.
	Source SkillSource
}

// SkillInvocation is the parsed result of executing a skill — exposed so
// callers (e.g. the conversation loop) can update active-skill state
// without re-reading and re-parsing the file.
type SkillInvocation struct {
	Resolution  SkillResolution
	Frontmatter SkillFrontmatter
	Description string
	Body        string
	Args        string
}

// SkillTool returns the api.Tool registration for the "skill" tool.
func SkillTool() api.Tool {
	return api.Tool{
		Name: "skill",
		Description: "Execute a skill by name — plain ('my-skill') or plugin-namespaced ('plugin:skill') — with optional args. " +
			"Skills are curated prompt files encoding a workflow or domain knowledge; when an available skill matches the user's request, invoke it BEFORE answering instead of improvising the procedure it encodes. " +
			"The skill body is returned to you to follow.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"skill": {Type: "string", Description: "The skill name to execute. Accepts 'name' or 'plugin:skill'."},
				"args":  {Type: "string", Description: "Optional arguments for the skill."},
			},
			Required: []string{"skill"},
		},
	}
}

// ExecuteSkill runs a skill on behalf of the model (LLM-invoked).
// Honors disable-model-invocation: true.
func ExecuteSkill(input map[string]any, workDir string) (string, error) {
	out, _, err := ExecuteSkillEx(input, workDir, false)
	return out, err
}

// ExecuteSkillUserInvoked runs a skill on behalf of the user (e.g. via
// /skills invoke). Bypasses the disable-model-invocation gate.
func ExecuteSkillUserInvoked(input map[string]any, workDir string) (string, error) {
	out, _, err := ExecuteSkillEx(input, workDir, true)
	return out, err
}

// ExecuteSkillEx runs a skill and returns both the JSON payload and the
// parsed invocation. The invocation is non-nil on success so callers can
// update derived state (e.g. active-skill restrictions) without re-reading
// the file.
func ExecuteSkillEx(input map[string]any, workDir string, invokedByUser bool) (string, *SkillInvocation, error) {
	skillName, ok := input["skill"].(string)
	if !ok || skillName == "" {
		return "", nil, fmt.Errorf("skill: 'skill' name is required")
	}
	args, _ := input["args"].(string)

	res, err := ResolveSkill(skillName, workDir)
	if err != nil {
		return "", nil, err
	}

	content, err := readSkillContent(res.Path)
	if err != nil {
		return "", nil, fmt.Errorf("skill: cannot read skill file %s: %v", res.Path, err)
	}

	front, body, hasFront := parseSkill(content)

	description := front.Description
	if description == "" {
		description = deriveLegacyDescription(body, res.CanonicalName)
	}

	if hasFront && front.DisableModelInvocation && !invokedByUser {
		return "", nil, fmt.Errorf("skill %q is marked disable-model-invocation; invoke explicitly via /skills invoke %s", res.CanonicalName, res.CanonicalName)
	}

	inv := &SkillInvocation{
		Resolution:  res,
		Frontmatter: front,
		Description: description,
		Body:        body,
		Args:        args,
	}

	result := map[string]any{
		"skill":         res.CanonicalName,
		"path":          res.Path,
		"source":        string(res.Source),
		"args":          args,
		"description":   description,
		"argument_hint": front.ArgumentHint,
		"allowed_tools": front.AllowedTools,
		"prompt":        body,
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), inv, nil
}

// ResolveSkill turns a user-supplied skill identifier into a concrete path,
// preferring filesystem-installed skills over bundled ones.
func ResolveSkill(name, workDir string) (SkillResolution, error) {
	plugin, skill, namespaced := splitSkillName(name)

	roots := skillLookupRoots(workDir)

	if namespaced {
		for _, root := range roots {
			candidates := []string{
				filepath.Join(root, plugin, "skills", skill, "SKILL.md"),
				filepath.Join(root, plugin, "skills", skill+".md"),
			}
			for _, p := range candidates {
				if info, err := os.Stat(p); err == nil && !info.IsDir() {
					return SkillResolution{
						CanonicalName: plugin + ":" + skill,
						Path:          p,
						Source:        SkillSourceFilesystem,
					}, nil
				}
			}
		}
		if bp, found := lookupBundledNamespaced(plugin, skill); found {
			return SkillResolution{
				CanonicalName: plugin + ":" + skill,
				Path:          bp,
				Source:        SkillSourceBundled,
			}, nil
		}
		return SkillResolution{}, fmt.Errorf("skill: %q not found in any search path", plugin+":"+skill)
	}

	// Flat (bare) name. Try legacy lookups first, then plugin-scoped walks.
	bare := strings.TrimSuffix(name, ".md")
	for _, root := range roots {
		candidates := []string{
			filepath.Join(root, bare+".md"),
			filepath.Join(root, bare, "SKILL.md"),
			filepath.Join(root, bare),
			filepath.Join(root, name),
		}
		for _, p := range candidates {
			if info, err := os.Stat(p); err == nil && !info.IsDir() {
				return SkillResolution{
					CanonicalName: bare,
					Path:          p,
					Source:        SkillSourceFilesystem,
				}, nil
			}
		}
	}

	// Walk plugin namespaces under each root.
	type match struct {
		plugin string
		path   string
	}
	var matches []match
	for _, root := range roots {
		plugins, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, p := range plugins {
			if !p.IsDir() {
				continue
			}
			candidate := filepath.Join(root, p.Name(), "skills", bare, "SKILL.md")
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				matches = append(matches, match{plugin: p.Name(), path: candidate})
			}
		}
		if len(matches) > 0 {
			break // first root with matches wins
		}
	}
	if len(matches) == 1 {
		return SkillResolution{
			CanonicalName: matches[0].plugin + ":" + bare,
			Path:          matches[0].path,
			Source:        SkillSourceFilesystem,
		}, nil
	}
	if len(matches) > 1 {
		names := make([]string, 0, len(matches))
		for _, m := range matches {
			names = append(names, m.plugin+":"+bare)
		}
		sort.Strings(names)
		return SkillResolution{}, fmt.Errorf("skill: %q is ambiguous; matches: %s", bare, strings.Join(names, ", "))
	}

	// Bundled fallback for bare names.
	if path, canonical, ambiguous, err := lookupBundledBare(bare); err != nil {
		return SkillResolution{}, err
	} else if len(ambiguous) > 1 {
		sort.Strings(ambiguous)
		return SkillResolution{}, fmt.Errorf("skill: %q is ambiguous in bundled skills; matches: %s", bare, strings.Join(ambiguous, ", "))
	} else if path != "" {
		return SkillResolution{
			CanonicalName: canonical,
			Path:          path,
			Source:        SkillSourceBundled,
		}, nil
	}

	return SkillResolution{}, fmt.Errorf("skill: %q not found in any search path", name)
}

// splitSkillName parses "plugin:skill" or "plugin/skill" into its parts.
func splitSkillName(name string) (plugin, skill string, namespaced bool) {
	if idx := strings.Index(name, ":"); idx > 0 && idx < len(name)-1 {
		return name[:idx], name[idx+1:], true
	}
	// Tolerate plugin/skill form (no leading slash, no .md).
	if idx := strings.Index(name, "/"); idx > 0 && idx < len(name)-1 && !strings.HasSuffix(name, ".md") && !strings.Contains(name[:idx], ".") {
		return name[:idx], name[idx+1:], true
	}
	return "", name, false
}

// parseSkill extracts the YAML frontmatter (if present) and returns the body.
// If the file does not begin with "---\n", the entire content is treated as
// the body and the returned frontmatter is the zero value.
// On malformed YAML, it silently falls back to the legacy mode.
func parseSkill(content []byte) (SkillFrontmatter, string, bool) {
	text := string(content)
	hasMarker := strings.HasPrefix(text, "---\n") || strings.HasPrefix(text, "---\r\n")
	if !hasMarker {
		return SkillFrontmatter{}, text, false
	}

	rest := text[strings.Index(text, "\n")+1:]
	// Find a "\n---" sequence where the closing marker is followed by
	// end-of-line or end-of-file — a literal "---" on its own line, not
	// "---foo" or "----".
	endIdx, after := findClosingDelimiter(rest)
	if endIdx < 0 {
		return SkillFrontmatter{}, text, false
	}

	yamlBlock := rest[:endIdx]
	body := rest[after:]
	body = strings.TrimPrefix(body, "\n")

	var front SkillFrontmatter
	if err := yaml.Unmarshal([]byte(yamlBlock), &front); err != nil {
		return SkillFrontmatter{}, text, false
	}
	return front, body, true
}

// findClosingDelimiter scans rest for the next "---" line. Returns the
// index where the YAML block ends (exclusive) and the index where the body
// begins. Returns -1, -1 if no such line exists.
func findClosingDelimiter(rest string) (yamlEnd, bodyStart int) {
	for i := 0; i+4 <= len(rest); {
		idx := strings.Index(rest[i:], "\n---")
		if idx < 0 {
			return -1, -1
		}
		absolute := i + idx
		tail := absolute + 4 // past "\n---"
		// Accept end-of-string, "\n" or "\r\n" immediately after the "---".
		if tail == len(rest) {
			return absolute, tail
		}
		switch rest[tail] {
		case '\n':
			return absolute, tail + 1
		case '\r':
			if tail+1 < len(rest) && rest[tail+1] == '\n' {
				return absolute, tail + 2
			}
		}
		i = tail
	}
	return -1, -1
}

// deriveLegacyDescription mimics the pre-frontmatter behavior: take the first
// non-empty line, stripped of leading '#'.
func deriveLegacyDescription(body, fallback string) string {
	if lines := strings.SplitN(body, "\n", 2); len(lines) > 0 {
		firstLine := strings.TrimSpace(lines[0])
		firstLine = strings.TrimLeft(firstLine, "#")
		firstLine = strings.TrimSpace(firstLine)
		if firstLine != "" {
			return firstLine
		}
	}
	return fallback
}

// readSkillContent reads from the embedded FS for embed:// URIs and from disk
// otherwise.
func readSkillContent(path string) ([]byte, error) {
	if strings.HasPrefix(path, embedScheme) {
		return fs.ReadFile(bundledSkillsFS, strings.TrimPrefix(path, embedScheme))
	}
	return os.ReadFile(path)
}

// SkillLookupRoots is the exported version of skillLookupRoots, used by the
// runtime package to enumerate skill sources for `/skills list`.
func SkillLookupRoots(workDir string) []string {
	return skillLookupRoots(workDir)
}

// skillLookupRoots returns directories to search for skills, in priority order.
func skillLookupRoots(workDir string) []string {
	var roots []string

	// Project-local skill directories (walk up from workDir).
	dir := workDir
	for {
		roots = append(roots,
			filepath.Join(dir, ".skills"),
			filepath.Join(dir, ".claude", "skills"),
			filepath.Join(dir, ".agents", "skills"),
			filepath.Join(dir, ".claw", "skills"),
		)
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	if configHome := os.Getenv("CLAW_CONFIG_HOME"); configHome != "" {
		roots = append(roots,
			filepath.Join(configHome, "skills"),
			filepath.Join(configHome, "commands"),
		)
	}
	if codexHome := os.Getenv("CODEX_HOME"); codexHome != "" {
		roots = append(roots,
			filepath.Join(codexHome, "skills"),
			filepath.Join(codexHome, "commands"),
		)
	}

	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots,
			filepath.Join(home, ".claude", "skills"),
			filepath.Join(home, ".claude", "skills", "omc-learned"),
			filepath.Join(home, ".claw", "skills"),
		)
		if claudeConfigDir := os.Getenv("CLAUDE_CONFIG_DIR"); claudeConfigDir != "" {
			roots = append(roots,
				filepath.Join(claudeConfigDir, "skills"),
				filepath.Join(claudeConfigDir, "commands"),
			)
		}
	}

	return roots
}

// ResolveSkillFrontmatter is a convenience used by callers (e.g. the
// conversation loop) that want to inspect a skill's metadata without
// executing it — e.g. to set the active skill's allowed-tools.
func ResolveSkillFrontmatter(name, workDir string) (SkillFrontmatter, SkillResolution, error) {
	res, err := ResolveSkill(name, workDir)
	if err != nil {
		return SkillFrontmatter{}, res, err
	}
	content, err := readSkillContent(res.Path)
	if err != nil {
		return SkillFrontmatter{}, res, err
	}
	front, _, _ := parseSkill(content)
	return front, res, nil
}
