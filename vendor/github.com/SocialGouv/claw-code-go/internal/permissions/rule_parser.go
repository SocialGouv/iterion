package permissions

import (
	"encoding/json"
	"strings"
)

// permissionRuleMatcherKind discriminates rule matcher variants.
type permissionRuleMatcherKind int

const (
	matcherAny    permissionRuleMatcherKind = iota // matches any subject
	matcherExact                                   // matches exact subject string
	matcherPrefix                                  // matches subject prefix (Bash(git:*) → "git")
)

// permissionRuleMatcher determines how a rule's subject pattern is evaluated.
type permissionRuleMatcher struct {
	kind  permissionRuleMatcherKind
	value string // used for matcherExact and matcherPrefix
}

// permissionRule stores a parsed permission rule. Rules follow the syntax
// "ToolName(subject_pattern)" where the subject_pattern can be:
//   - empty or "*" → matcherAny
//   - "prefix:*"   → matcherPrefix
//   - "exact"      → matcherExact
//
// Example: "Bash(git:*)" matches tool "Bash" with any command starting with "git".
// Escaped parentheses (\( and \)) are supported.
type permissionRule struct {
	raw      string
	toolName string
	matcher  permissionRuleMatcher
}

// parsePermissionRule parses a rule string into a permissionRule.
// Matches Rust's PermissionRule::parse.
func parsePermissionRule(raw string) permissionRule {
	trimmed := strings.TrimSpace(raw)
	open := findFirstUnescaped(trimmed, '(')
	close := findLastUnescaped(trimmed, ')')

	if open >= 0 && close >= 0 && close == len(trimmed)-1 && open < close {
		toolName := strings.TrimSpace(trimmed[:open])
		content := trimmed[open+1 : close]
		if toolName != "" {
			return permissionRule{
				raw:      trimmed,
				toolName: toolName,
				matcher:  parseRuleMatcher(content),
			}
		}
	}

	return permissionRule{
		raw:      trimmed,
		toolName: trimmed,
		matcher:  permissionRuleMatcher{kind: matcherAny},
	}
}

// parseRuleMatcher parses the content inside parentheses into a matcher.
func parseRuleMatcher(content string) permissionRuleMatcher {
	unescaped := unescapeRuleContent(strings.TrimSpace(content))
	if unescaped == "" || unescaped == "*" {
		return permissionRuleMatcher{kind: matcherAny}
	}
	if prefix, ok := strings.CutSuffix(unescaped, ":*"); ok {
		return permissionRuleMatcher{kind: matcherPrefix, value: prefix}
	}
	return permissionRuleMatcher{kind: matcherExact, value: unescaped}
}

// unescapeRuleContent replaces escaped characters in rule content.
func unescapeRuleContent(content string) string {
	s := strings.ReplaceAll(content, `\(`, "(")
	s = strings.ReplaceAll(s, `\)`, ")")
	s = strings.ReplaceAll(s, `\\`, `\`)
	return s
}

// matches returns true if the rule matches the given tool name and input.
func (r *permissionRule) matches(toolName, input string) bool {
	if r.toolName != toolName {
		return false
	}
	switch r.matcher.kind {
	case matcherAny:
		return true
	case matcherExact:
		subject := extractPermissionSubject(input)
		return subject != "" && subject == r.matcher.value
	case matcherPrefix:
		subject := extractPermissionSubject(input)
		return subject != "" && strings.HasPrefix(subject, r.matcher.value)
	}
	return false
}

// extractPermissionSubject extracts the permission-relevant subject string from
// a tool input. If the input is valid JSON, it checks keys in priority order
// (command, path, file_path, filePath, notebook_path, notebookPath, url,
// pattern, code, message). Falls back to the raw input string if not JSON.
// Matches Rust's extract_permission_subject.
func extractPermissionSubject(input string) string {
	var obj map[string]any
	if err := json.Unmarshal([]byte(input), &obj); err == nil {
		for _, key := range []string{
			"command", "path", "file_path", "filePath",
			"notebook_path", "notebookPath", "url",
			"pattern", "code", "message",
		} {
			if v, ok := obj[key]; ok {
				if s, ok := v.(string); ok {
					return s
				}
			}
		}
	}

	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return ""
	}
	return input
}

// findFirstUnescaped returns the index of the first unescaped occurrence of
// needle in value, or -1 if not found. Matches Rust's find_first_unescaped.
func findFirstUnescaped(value string, needle rune) int {
	escaped := false
	for i, ch := range value {
		if ch == '\\' {
			escaped = !escaped
			continue
		}
		if ch == needle && !escaped {
			return i
		}
		escaped = false
	}
	return -1
}

// findLastUnescaped returns the index of the last unescaped occurrence of
// needle in value, or -1 if not found. Matches Rust's find_last_unescaped.
func findLastUnescaped(value string, needle rune) int {
	runes := []rune(value)
	for i := len(runes) - 1; i >= 0; i-- {
		if runes[i] != needle {
			continue
		}
		backslashes := 0
		for j := i - 1; j >= 0; j-- {
			if runes[j] == '\\' {
				backslashes++
			} else {
				break
			}
		}
		if backslashes%2 == 0 {
			// Convert rune index to byte index
			byteIdx := 0
			for ri := 0; ri < i; ri++ {
				byteIdx += len(string(runes[ri]))
			}
			return byteIdx
		}
	}
	return -1
}
