package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/SocialGouv/claw-code-go/internal/strutil"
)

// FrontmatterConfig holds config overrides parsed from CLAUDE.md YAML frontmatter.
type FrontmatterConfig struct {
	Model          *string  `json:"model,omitempty"`
	PermissionMode *string  `json:"permissionMode,omitempty"`
	AllowedTools   []string `json:"allowedTools,omitempty"`

	// MinimalPrompt flips the default of every system-prompt section to off
	// (same semantics as the settings "prompt.minimal" key).
	MinimalPrompt *bool `json:"minimalPrompt,omitempty"`
	// PromptSections enables ONLY the listed sections (implies MinimalPrompt).
	PromptSections []string `json:"promptSections,omitempty"`
	// DisablePromptSections disables the listed sections; others keep defaults.
	DisablePromptSections []string `json:"disablePromptSections,omitempty"`
}

// HasOverrides returns true if any override field is set.
func (c FrontmatterConfig) HasOverrides() bool {
	return c.Model != nil || c.PermissionMode != nil || len(c.AllowedTools) > 0 ||
		c.MinimalPrompt != nil || len(c.PromptSections) > 0 || len(c.DisablePromptSections) > 0
}

// LoadFrontmatterForDir reads <dir>/CLAUDE.md and parses its YAML frontmatter.
// Returns nil when the file is missing, unparseable, or carries no overrides.
func LoadFrontmatterForDir(dir string) *FrontmatterConfig {
	data, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if err != nil {
		return nil
	}
	fm, _, err := ParseFrontmatter(data)
	if err != nil || !fm.HasOverrides() {
		return nil
	}
	return &fm
}

// ParseFrontmatter extracts YAML frontmatter from CLAUDE.md content.
// Frontmatter is delimited by "---\n" at the start and a closing "---\n" (or
// "---" at EOF). Between the delimiters, simple "key: value" lines are parsed
// (one level deep, no nesting). For list-valued keys (allowedTools,
// promptSections, disablePromptSections), YAML list items ("- item") and
// inline comma-separated values are supported. Returns the parsed config, the
// remaining body after the frontmatter block, and any error.
//
// If no frontmatter is present, returns a zero FrontmatterConfig and the full
// content unchanged.
func ParseFrontmatter(content []byte) (FrontmatterConfig, []byte, error) {
	var cfg FrontmatterConfig

	// Must start with "---\n"
	if !bytes.HasPrefix(content, []byte("---\n")) {
		return cfg, content, nil
	}

	// Find closing "---\n" or "---" at EOF (skip the opening delimiter).
	rest := content[4:]
	closeIdx := bytes.Index(rest, []byte("---\n"))
	if closeIdx < 0 {
		// Check for "---" at EOF (no trailing newline).
		if bytes.HasSuffix(rest, []byte("---")) {
			closeIdx = len(rest) - 3
		} else {
			return cfg, nil, fmt.Errorf("frontmatter: no closing delimiter found")
		}
	}

	frontmatterBlock := rest[:closeIdx]
	body := rest[closeIdx+3:] // skip "---"
	if len(body) > 0 && body[0] == '\n' {
		body = body[1:]
	}

	// Parse simple key: value lines.
	var currentKey string
	var listItems []string
	inList := false

	assignList := func(key string, items []string) {
		switch key {
		case "allowedTools":
			cfg.AllowedTools = items
		case "promptSections":
			cfg.PromptSections = items
		case "disablePromptSections":
			cfg.DisablePromptSections = items
		}
	}

	flushList := func() {
		if inList {
			assignList(currentKey, listItems)
		}
		inList = false
		currentKey = ""
		listItems = nil
	}

	lines := strings.Split(string(frontmatterBlock), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		// Check for list item.
		if strings.HasPrefix(trimmed, "- ") && inList {
			listItems = append(listItems, strings.TrimSpace(trimmed[2:]))
			continue
		}

		// Otherwise it's a key: value line. Flush any previous list.
		flushList()

		colonIdx := strings.Index(trimmed, ":")
		if colonIdx < 0 {
			continue
		}

		key := strings.TrimSpace(trimmed[:colonIdx])
		value := strings.TrimSpace(trimmed[colonIdx+1:])

		if value == "" {
			// Could be start of a list (e.g., "allowedTools:")
			currentKey = key
			inList = true
			continue
		}

		switch key {
		case "model":
			v := value
			cfg.Model = &v
		case "permissionMode":
			v := value
			cfg.PermissionMode = &v
		case "minimalPrompt":
			if b, err := strconv.ParseBool(value); err == nil {
				cfg.MinimalPrompt = &b
			}
		case "allowedTools", "promptSections", "disablePromptSections":
			// Inline value: single item or comma-separated (optionally
			// bracketed YAML flow style, e.g. "[a, b]").
			assignList(key, splitInlineList(value))
		}
	}
	flushList()

	return cfg, body, nil
}

// splitInlineList splits an inline YAML-ish list value ("a, b" or "[a, b]")
// into trimmed non-empty items.
func splitInlineList(value string) []string {
	return strutil.SplitComma(strings.TrimSuffix(strings.TrimPrefix(value, "["), "]"))
}
