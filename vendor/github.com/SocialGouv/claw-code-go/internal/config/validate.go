package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// DiagnosticKind describes the type of a config diagnostic.
type DiagnosticKind interface {
	Kind() string
	String() string
}

// UnknownKeyDiag is emitted when a config key is not recognized.
type UnknownKeyDiag struct {
	Suggestion string // closest known key (empty if none close enough)
}

func (d UnknownKeyDiag) Kind() string { return "unknown_key" }
func (d UnknownKeyDiag) String() string {
	if d.Suggestion != "" {
		return fmt.Sprintf("unknown key (did you mean %q?)", d.Suggestion)
	}
	return "unknown key"
}

// WrongTypeDiag is emitted when a config value has the wrong JSON type.
type WrongTypeDiag struct {
	Expected string
	Got      string
}

func (d WrongTypeDiag) Kind() string { return "wrong_type" }
func (d WrongTypeDiag) String() string {
	return fmt.Sprintf("expected %s, got %s", d.Expected, d.Got)
}

// DeprecatedDiag is emitted when a deprecated config key is used.
type DeprecatedDiag struct {
	Replacement string
}

func (d DeprecatedDiag) Kind() string { return "deprecated" }
func (d DeprecatedDiag) String() string {
	return fmt.Sprintf("deprecated, use %q instead", d.Replacement)
}

// ConfigDiagnostic is a single diagnostic about a settings file.
type ConfigDiagnostic struct {
	Path  string         // file path
	Field string         // JSON key path (e.g. "hooks.PreToolUse")
	Line  int            // 0 means unknown
	Kind  DiagnosticKind // one of UnknownKeyDiag, WrongTypeDiag, DeprecatedDiag
}

func (d ConfigDiagnostic) String() string {
	loc := d.Path
	if d.Line > 0 {
		loc = fmt.Sprintf("%s:%d", d.Path, d.Line)
	}
	return fmt.Sprintf("%s: field %q: %s", loc, d.Field, d.Kind)
}

// ValidationResult holds all diagnostics from validating a settings file.
type ValidationResult struct {
	Errors   []ConfigDiagnostic
	Warnings []ConfigDiagnostic
}

// HasErrors returns true if there are any error-level diagnostics.
func (r *ValidationResult) HasErrors() bool { return len(r.Errors) > 0 }

// HasWarnings returns true if there are any warning-level diagnostics.
func (r *ValidationResult) HasWarnings() bool { return len(r.Warnings) > 0 }

// IsClean returns true if there are no diagnostics at all.
func (r *ValidationResult) IsClean() bool { return !r.HasErrors() && !r.HasWarnings() }

// fieldSpec describes a known configuration field.
type fieldSpec struct {
	name     string
	jsonType string // "string", "number", "boolean", "object", "array"
}

// Top-level known fields in settings.json.
var topLevelFields = []fieldSpec{
	{"$schema", "string"},
	{"model", "string"},
	{"hooks", "object"},
	{"permissions", "object"},
	{"permissionMode", "string"},
	{"mcpServers", "object"},
	{"oauth", "object"},
	{"enabledPlugins", "object"},
	{"plugins", "object"},
	{"sandbox", "object"},
	{"env", "object"},
	{"aliases", "object"},
	{"providerFallbacks", "object"},
	{"trustedRoots", "array"},
	{"maxTokens", "number"},
	{"theme", "string"},
	{"allowedTools", "array"},
	{"blockedTools", "array"},
}

var hooksFields = []fieldSpec{
	{"PreToolUse", "array"},
	{"PostToolUse", "array"},
	{"PostToolUseFailure", "array"},
}

var permissionsFields = []fieldSpec{
	{"defaultMode", "string"},
	{"allow", "array"},
	{"deny", "array"},
	{"ask", "array"},
}

var pluginsFields = []fieldSpec{
	{"enabled", "object"},
	{"externalDirectories", "array"},
	{"installRoot", "string"},
	{"registryPath", "string"},
	{"bundledRoot", "string"},
	{"maxOutputTokens", "number"},
}

var sandboxFields = []fieldSpec{
	{"enabled", "boolean"},
	{"namespaceRestrictions", "boolean"},
	{"networkIsolation", "boolean"},
	{"filesystemMode", "string"},
	{"allowedMounts", "array"},
}

var oauthFields = []fieldSpec{
	{"clientId", "string"},
	{"authorizeUrl", "string"},
	{"tokenUrl", "string"},
	{"callbackPort", "number"},
	{"manualRedirectUrl", "string"},
	{"scopes", "array"},
}

var mcpServerEntryFields = []fieldSpec{
	{"name", "string"},
	{"transport", "string"},
	{"command", "string"},
	{"args", "array"},
	{"url", "string"},
	{"env", "object"},
}

// Deprecated field mappings (old key → replacement).
var deprecatedFields = map[string]string{
	"permissionMode": "permissions.defaultMode",
	"enabledPlugins": "plugins.enabled",
}

// ValidateSettingsJSON validates a raw JSON settings file.
// filePath is used for diagnostic messages only.
func ValidateSettingsJSON(data []byte, filePath string) ValidationResult {
	var result ValidationResult

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		result.Errors = append(result.Errors, ConfigDiagnostic{
			Path:  filePath,
			Field: "<root>",
			Kind:  WrongTypeDiag{Expected: "JSON object", Got: "invalid JSON"},
		})
		return result
	}

	validateObjectKeys(raw, topLevelFields, "", filePath, data, &result)

	// Check deprecated fields.
	for key, replacement := range deprecatedFields {
		if _, ok := raw[key]; ok {
			result.Warnings = append(result.Warnings, ConfigDiagnostic{
				Path:  filePath,
				Field: key,
				Line:  findKeyLine(data, key),
				Kind:  DeprecatedDiag{Replacement: replacement},
			})
		}
	}

	// Validate nested objects.
	if hooksRaw, ok := raw["hooks"]; ok {
		var hooks map[string]json.RawMessage
		if json.Unmarshal(hooksRaw, &hooks) == nil {
			validateObjectKeys(hooks, hooksFields, "hooks.", filePath, data, &result)
		} else {
			result.Errors = append(result.Errors, ConfigDiagnostic{
				Path:  filePath,
				Field: "hooks",
				Line:  findKeyLine(data, "hooks"),
				Kind:  WrongTypeDiag{Expected: "object", Got: jsonTypeName(hooksRaw)},
			})
		}
	}

	if permRaw, ok := raw["permissions"]; ok {
		var perms map[string]json.RawMessage
		if json.Unmarshal(permRaw, &perms) == nil {
			validateObjectKeys(perms, permissionsFields, "permissions.", filePath, data, &result)
		} else {
			result.Errors = append(result.Errors, ConfigDiagnostic{
				Path:  filePath,
				Field: "permissions",
				Line:  findKeyLine(data, "permissions"),
				Kind:  WrongTypeDiag{Expected: "object", Got: jsonTypeName(permRaw)},
			})
		}
	}

	if pluginsRaw, ok := raw["plugins"]; ok {
		var plugins map[string]json.RawMessage
		if json.Unmarshal(pluginsRaw, &plugins) == nil {
			validateObjectKeys(plugins, pluginsFields, "plugins.", filePath, data, &result)
		} else {
			result.Errors = append(result.Errors, ConfigDiagnostic{
				Path:  filePath,
				Field: "plugins",
				Line:  findKeyLine(data, "plugins"),
				Kind:  WrongTypeDiag{Expected: "object", Got: jsonTypeName(pluginsRaw)},
			})
		}
	}

	if sandboxRaw, ok := raw["sandbox"]; ok {
		var sandbox map[string]json.RawMessage
		if json.Unmarshal(sandboxRaw, &sandbox) == nil {
			validateObjectKeys(sandbox, sandboxFields, "sandbox.", filePath, data, &result)
		} else {
			result.Errors = append(result.Errors, ConfigDiagnostic{
				Path:  filePath,
				Field: "sandbox",
				Line:  findKeyLine(data, "sandbox"),
				Kind:  WrongTypeDiag{Expected: "object", Got: jsonTypeName(sandboxRaw)},
			})
		}
	}

	if oauthRaw, ok := raw["oauth"]; ok {
		var oauth map[string]json.RawMessage
		if json.Unmarshal(oauthRaw, &oauth) == nil {
			validateObjectKeys(oauth, oauthFields, "oauth.", filePath, data, &result)
		} else {
			result.Errors = append(result.Errors, ConfigDiagnostic{
				Path:  filePath,
				Field: "oauth",
				Line:  findKeyLine(data, "oauth"),
				Kind:  WrongTypeDiag{Expected: "object", Got: jsonTypeName(oauthRaw)},
			})
		}
	}

	// Validate individual MCP server entries.
	if mcpRaw, ok := raw["mcpServers"]; ok {
		var servers map[string]json.RawMessage
		if json.Unmarshal(mcpRaw, &servers) == nil {
			for serverName, serverRaw := range servers {
				var entry map[string]json.RawMessage
				if json.Unmarshal(serverRaw, &entry) == nil {
					validateObjectKeys(entry, mcpServerEntryFields, "mcpServers."+serverName+".", filePath, data, &result)
				}
			}
		}
	}

	// Type-check top-level fields.
	for _, spec := range topLevelFields {
		val, ok := raw[spec.name]
		if !ok {
			continue
		}
		gotType := jsonTypeName(val)
		if gotType != spec.jsonType && gotType != "null" {
			result.Errors = append(result.Errors, ConfigDiagnostic{
				Path:  filePath,
				Field: spec.name,
				Line:  findKeyLine(data, spec.name),
				Kind:  WrongTypeDiag{Expected: spec.jsonType, Got: gotType},
			})
		}
	}

	return result
}

// FormatDiagnostics returns a human-readable string of all diagnostics.
func FormatDiagnostics(result *ValidationResult) string {
	var sb strings.Builder
	for _, d := range result.Errors {
		fmt.Fprintf(&sb, "error: %s\n", d)
	}
	for _, d := range result.Warnings {
		fmt.Fprintf(&sb, "warning: %s\n", d)
	}
	return sb.String()
}

func validateObjectKeys(obj map[string]json.RawMessage, known []fieldSpec, prefix, filePath string, source []byte, result *ValidationResult) {
	knownNames := make([]string, len(known))
	knownSet := make(map[string]bool, len(known))
	for i, f := range known {
		knownNames[i] = f.name
		knownSet[f.name] = true
	}

	for key := range obj {
		if knownSet[key] {
			continue
		}
		// Skip deprecated fields (handled separately).
		if _, isDeprecated := deprecatedFields[key]; isDeprecated && prefix == "" {
			continue
		}
		diag := ConfigDiagnostic{
			Path:  filePath,
			Field: prefix + key,
			Line:  findKeyLine(source, key),
		}
		suggestion := suggestField(key, knownNames)
		diag.Kind = UnknownKeyDiag{Suggestion: suggestion}
		result.Errors = append(result.Errors, diag)
	}
}

// suggestField returns the closest known field name if edit distance <= 3
// and the key length >= 4 (to avoid false positives on short keys).
// The len < 4 guard is intentional: very short unknown keys (e.g., "id")
// would produce spurious suggestions against unrelated longer field names.
func suggestField(input string, candidates []string) string {
	if len(input) < 4 {
		return ""
	}
	inputLower := strings.ToLower(input)
	bestDist := 4 // only suggest if distance <= 3
	bestCandidate := ""
	for _, c := range candidates {
		d := levenshteinDistance(inputLower, strings.ToLower(c))
		if d < bestDist {
			bestDist = d
			bestCandidate = c
		}
	}
	return bestCandidate
}

// levenshteinDistance computes the Levenshtein edit distance between two strings.
func levenshteinDistance(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}

	prev := make([]int, lb+1)
	curr := make([]int, lb+1)

	for j := 0; j <= lb; j++ {
		prev[j] = j
	}

	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(
				prev[j]+1,      // deletion
				curr[j-1]+1,    // insertion
				prev[j-1]+cost, // substitution
			)
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

func min(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// findKeyLine returns the 1-based line number where a JSON key first appears.
func findKeyLine(source []byte, key string) int {
	target := fmt.Sprintf("%q", key)
	lines := strings.Split(string(source), "\n")
	for i, line := range lines {
		if strings.Contains(line, target) {
			return i + 1
		}
	}
	return 0
}

// ValidateConfigFile reads and validates a settings file, returning diagnostics.
func ValidateConfigFile(path string) ValidationResult {
	data, err := os.ReadFile(path)
	if err != nil {
		return ValidationResult{
			Errors: []ConfigDiagnostic{{
				Path:  path,
				Field: "<file>",
				Kind:  WrongTypeDiag{Expected: "readable file", Got: err.Error()},
			}},
		}
	}
	if unsupported := checkUnsupportedFormat(data); unsupported != "" {
		return ValidationResult{
			Errors: []ConfigDiagnostic{{
				Path:  path,
				Field: "<root>",
				Kind:  WrongTypeDiag{Expected: "JSON object", Got: unsupported},
			}},
		}
	}
	return ValidateSettingsJSON(data, path)
}

// checkUnsupportedFormat detects non-JSON config formats and returns a
// description of the detected format, or "" if it looks like valid JSON.
func checkUnsupportedFormat(data []byte) string {
	s := strings.TrimSpace(string(data))
	if len(s) == 0 {
		return ""
	}
	// YAML detection: starts with "---" or contains "key: value" patterns
	if strings.HasPrefix(s, "---") {
		return "YAML format (only JSON is supported)"
	}
	// TOML detection: starts with "[section]" or "key = value"
	if len(s) > 0 && s[0] == '[' {
		// Could be JSON array — let JSON parser handle that
		return ""
	}
	// INI-style detection
	if strings.Contains(s, " = ") && !strings.HasPrefix(s, "{") {
		// Heuristic: non-JSON with "key = value" pattern
		return "INI/TOML format (only JSON is supported)"
	}
	return ""
}

// jsonTypeName returns the JSON type name for a raw JSON value.
func jsonTypeName(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if len(s) == 0 {
		return "null"
	}
	switch s[0] {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	default:
		// number
		return "number"
	}
}
