package config

import (
	"bytes"
	"fmt"
	"strings"
)

// FrontmatterConfig holds config overrides parsed from CLAUDE.md YAML frontmatter.
type FrontmatterConfig struct {
	Model          *string  `json:"model,omitempty"`
	PermissionMode *string  `json:"permissionMode,omitempty"`
	AllowedTools   []string `json:"allowedTools,omitempty"`
}

// ParseFrontmatter extracts YAML frontmatter from CLAUDE.md content.
// Frontmatter is delimited by "---\n" at the start and a closing "---\n" (or
// "---" at EOF). Between the delimiters, simple "key: value" lines are parsed
// (one level deep, no nesting). For allowedTools, YAML list items ("- item")
// are supported. Returns the parsed config, the remaining body after the
// frontmatter block, and any error.
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

	flushList := func() {
		if inList && currentKey == "allowedTools" {
			cfg.AllowedTools = listItems
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
		case "allowedTools":
			// Inline single value (unusual but handle it).
			cfg.AllowedTools = []string{value}
		}
	}
	flushList()

	return cfg, body, nil
}
