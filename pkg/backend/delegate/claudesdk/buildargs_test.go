package claudesdk

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// flagValue returns the argument following the first occurrence of flag in
// args, or "" if the flag is absent or has no value after it.
func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func hasFlag(args []string, flag string) bool {
	return slices.Contains(args, flag)
}

func TestBuildArgs_AppendSystemPromptNotReplace(t *testing.T) {
	// The claude_code backend routes the assembled prompt to
	// --append-system-prompt so Claude Code's native system prompt is kept.
	args := buildArgs(processConfig{AppendSystemPrompt: "extra instructions"}, true)
	if got := flagValue(args, "--append-system-prompt"); got != "extra instructions" {
		t.Errorf("--append-system-prompt = %q, want %q", got, "extra instructions")
	}
	if hasFlag(args, "--system-prompt") {
		t.Error("--system-prompt must not be emitted when only AppendSystemPrompt is set (would replace the native prompt)")
	}
}

func TestBuildArgs_SettingSources(t *testing.T) {
	args := buildArgs(processConfig{
		SettingSources: []SettingSource{SettingSourceUser, SettingSourceProject},
	}, true)
	if got := flagValue(args, "--setting-sources"); got != "user,project" {
		t.Errorf("--setting-sources = %q, want %q", got, "user,project")
	}

	// No sources → flag omitted (CLI falls back to its own default).
	if hasFlag(buildArgs(processConfig{}, true), "--setting-sources") {
		t.Error("--setting-sources must be omitted when no sources are configured")
	}

	// An explicitly empty list loads NO source, and must still be emitted:
	// omitted, the CLI would load every source, `local` included.
	none := buildArgs(processConfig{SettingSources: []SettingSource{}}, true)
	if !hasFlag(none, "--setting-sources") {
		t.Fatal("an empty, non-nil list must emit --setting-sources")
	}
	if got := flagValue(none, "--setting-sources"); got != "" {
		t.Errorf("--setting-sources = %q, want the empty string", got)
	}
}

func TestBuildArgs_ThinkingDisplay(t *testing.T) {
	args := buildArgs(processConfig{ThinkingDisplay: "summarized"}, true)
	if got := flagValue(args, "--thinking-display"); got != "summarized" {
		t.Errorf("--thinking-display = %q, want summarized", got)
	}

	// Empty → flag omitted (older CLIs reject unknown options).
	if hasFlag(buildArgs(processConfig{}, true), "--thinking-display") {
		t.Error("--thinking-display must be omitted when unset")
	}
}

func TestBuildArgs_SettingSourcesAllThree(t *testing.T) {
	args := buildArgs(processConfig{
		SettingSources: []SettingSource{SettingSourceUser, SettingSourceProject, SettingSourceLocal},
	}, true)
	got := flagValue(args, "--setting-sources")
	for _, want := range []string{"user", "project", "local"} {
		if !strings.Contains(got, want) {
			t.Errorf("--setting-sources %q missing %q", got, want)
		}
	}
}

func TestBuildArgs_StrictMCPConfig(t *testing.T) {
	if !hasFlag(buildArgs(processConfig{StrictMCPConfig: true}, true), "--strict-mcp-config") {
		t.Error("--strict-mcp-config must be emitted when StrictMCPConfig is set")
	}
	// Emitted with no --mcp-config too: "no servers at all" is the point —
	// the CLI must not fall back to user/project MCP scopes.
	if !hasFlag(buildArgs(processConfig{StrictMCPConfig: true}, false), "--strict-mcp-config") {
		t.Error("--strict-mcp-config must be emitted on the one-shot path as well")
	}
	if hasFlag(buildArgs(processConfig{}, true), "--strict-mcp-config") {
		t.Error("--strict-mcp-config must be omitted when unset (host inheritance opt-in)")
	}
}

func TestBuildArgs_Settings(t *testing.T) {
	args := buildArgs(processConfig{
		SettingsJSON: []byte(`{"autoMemoryEnabled":true}`),
	}, true)
	if got := flagValue(args, "--settings"); got != `{"autoMemoryEnabled":true}` {
		t.Errorf("--settings = %q, want the inline JSON verbatim", got)
	}

	// Empty → flag omitted. The CLI treats an empty --settings as a parse
	// error, and there is nothing to merge anyway.
	if hasFlag(buildArgs(processConfig{}, true), "--settings") {
		t.Error("--settings must be omitted when no inline settings are configured")
	}
}

func TestBuildArgs_DisallowedTools(t *testing.T) {
	args := buildArgs(processConfig{DisallowedTools: []string{"Bash", "Write"}}, true)
	if got := flagValue(args, "--disallowedTools"); got != "Bash,Write" {
		t.Errorf("--disallowedTools = %q, want Bash,Write", got)
	}
	if hasFlag(buildArgs(processConfig{}, true), "--disallowedTools") {
		t.Error("--disallowedTools must be omitted when no native tools are restricted")
	}
}

// Both tool flags are SETS, and both options APPEND, so a caller composing
// several bounds onto one spawn legitimately produces the same name twice —
// the gated structured-output pass withholds a roster that already carries
// `Task` plus an orchestration list that carries it too. The argv must still
// name it once, in first-occurrence order (the lists are read in logs and
// pinned by tests, so a sorted result would churn both).
func TestBuildArgs_DedupesBothToolFlagsKeepingFirstOccurrenceOrder(t *testing.T) {
	args := buildArgs(processConfig{
		AllowedTools:    []string{"Read", "Glob", "Read", "Bash"},
		DisallowedTools: []string{"Task", "Agent", "Task", "Workflow", "Agent"},
	}, false)
	if got := flagValue(args, "--allowedTools"); got != "Read,Glob,Bash" {
		t.Errorf("--allowedTools = %q, want Read,Glob,Bash", got)
	}
	if got := flagValue(args, "--disallowedTools"); got != "Task,Agent,Workflow" {
		t.Errorf("--disallowedTools = %q, want Task,Agent,Workflow", got)
	}
}

func TestBuildArgs_JSONFieldUsesClaudeAnySchema(t *testing.T) {
	for _, tc := range []struct {
		name      string
		streaming bool
	}{
		{name: "main pass", streaming: true},
		{name: "formatting pass", streaming: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := buildArgs(processConfig{OutputFormat: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"created": map[string]any{
						"type": []any{"null", "string", "object", "boolean", "array", "number"},
					},
					"payload": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"nested": map[string]any{
								"type": []any{"object", "array", "string", "number", "boolean", "null"},
							},
						},
					},
					"summary": map[string]any{"type": "string"},
				},
				"required":             []any{"created", "summary"},
				"additionalProperties": false,
			}}, tc.streaming)

			raw := flagValue(args, "--json-schema")
			if raw == "" {
				t.Fatal("--json-schema missing from argv")
			}
			var schema struct {
				Properties map[string]map[string]any `json:"properties"`
			}
			if err := json.Unmarshal([]byte(raw), &schema); err != nil {
				t.Fatalf("unmarshal --json-schema: %v", err)
			}
			created, ok := schema.Properties["created"]
			if !ok {
				t.Fatalf("created property missing from --json-schema: %s", raw)
			}
			if len(created) != 0 {
				t.Errorf("created schema = %#v, want the empty any schema; argv JSON: %s", created, raw)
			}
			payload := schema.Properties["payload"]
			nestedProperties, _ := payload["properties"].(map[string]any)
			nested, _ := nestedProperties["nested"].(map[string]any)
			if len(nested) != 0 {
				t.Errorf("nested schema = %#v, want the empty any schema; argv JSON: %s", nested, raw)
			}
			if got := schema.Properties["summary"]["type"]; got != "string" {
				t.Errorf("summary type = %v, want string; argv JSON: %s", got, raw)
			}
		})
	}
}

func TestBuildArgs_JSONFieldSchemaPositions(t *testing.T) {
	type fixture struct {
		name   string
		schema map[string]any
	}
	tests := []fixture{{name: "root", schema: anyJSONSchemaFixture()}}
	for _, key := range []string{
		"additionalItems", "additionalProperties", "contains", "contentSchema",
		"else", "extends", "if", "items", "not", "propertyNames", "then",
		"unevaluatedItems", "unevaluatedProperties",
	} {
		tests = append(tests, fixture{name: "value/" + key, schema: map[string]any{key: anyJSONSchemaFixture()}})
	}
	for _, key := range []string{
		"$defs", "definitions", "dependencies", "dependentSchemas", "patternProperties", "properties",
	} {
		tests = append(tests, fixture{
			name:   "map/" + key,
			schema: map[string]any{key: map[string]any{"nested": anyJSONSchemaFixture()}},
		})
	}
	for _, key := range []string{"allOf", "anyOf", "extends", "items", "oneOf", "prefixItems"} {
		tests = append(tests, fixture{name: "list/" + key, schema: map[string]any{key: []any{anyJSONSchemaFixture()}}})
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := countTypeUnions(tc.schema); got != 1 {
				t.Fatalf("fixture contains %d type unions, want 1", got)
			}
			raw := outputSchemaArg(t, tc.schema)
			var decoded any
			if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
				t.Fatalf("unmarshal --json-schema: %v", err)
			}
			if got := countTypeUnions(decoded); got != 0 {
				t.Errorf("argv schema retains %d type unions: %s", got, raw)
			}
		})
	}
}

func TestBuildArgs_JSONFieldSchemaValuesAndOtherUnionsUnchanged(t *testing.T) {
	schema := map[string]any{
		"enum":              []any{anyJSONSchemaFixture()},
		"const":             anyJSONSchemaFixture(),
		"default":           anyJSONSchemaFixture(),
		"examples":          []any{anyJSONSchemaFixture()},
		"maximum":           json.Number("9007199254740993"),
		"required":          []any{"summary"},
		"dependencies":      map[string]any{"created": []any{"summary"}},
		"dependentRequired": map[string]any{"created": []any{"summary"}},
		"properties": map[string]any{
			"nullable": map[string]any{"type": []any{"string", "null"}},
			"duplicate": map[string]any{
				"type": []any{"object", "array", "string", "number", "boolean", "boolean"},
			},
		},
	}
	before, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	if raw := outputSchemaArg(t, schema); raw != string(before) {
		t.Errorf("argv schema changed JSON values or a non-any union:\n got %s\nwant %s", raw, before)
	}
	after, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("source schema mutated:\n got %s\nwant %s", after, before)
	}
}

func TestBuildArgs_JSONFieldSchemaSourceIsNotMutated(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"created": anyJSONSchemaFixture(),
		},
	}
	before, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	if raw := outputSchemaArg(t, schema); raw == string(before) {
		t.Fatal("fixture was not normalized")
	}
	after, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("source schema mutated:\n got %s\nwant %s", after, before)
	}
}

func TestBuildArgs_CyclicJSONSchemaDoesNotStackOverflow(t *testing.T) {
	schema := map[string]any{"type": "object"}
	schema["properties"] = map[string]any{"self": schema}
	args := buildArgs(processConfig{OutputFormat: schema}, false)
	if hasFlag(args, "--json-schema") {
		t.Error("cyclic schema must keep the existing marshal-error behavior and omit --json-schema")
	}
}

func TestBuildArgs_JSONFieldTypeUnionDuplicates(t *testing.T) {
	for _, tc := range []struct {
		name    string
		types   []any
		wantAny bool
	}{
		{
			name:    "duplicate with all six types",
			types:   []any{"object", "object", "array", "string", "number", "boolean", "null"},
			wantAny: true,
		},
		{
			name:    "duplicate with missing type",
			types:   []any{"object", "object", "array", "string", "number", "boolean"},
			wantAny: false,
		},
		{
			name:    "unknown type",
			types:   []any{"object", "array", "string", "number", "boolean", "null", "bogus"},
			wantAny: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := outputSchemaArg(t, map[string]any{
				"type": "object",
				"properties": map[string]any{
					"value": map[string]any{"type": tc.types},
				},
			})
			var schema struct {
				Properties map[string]map[string]any `json:"properties"`
			}
			if err := json.Unmarshal([]byte(raw), &schema); err != nil {
				t.Fatalf("unmarshal --json-schema: %v", err)
			}
			property := schema.Properties["value"]
			_, hasType := property["type"]
			if tc.wantAny && len(property) != 0 {
				t.Errorf("union with duplicates schema = %#v, want the empty any schema: %s", property, raw)
			}
			if !tc.wantAny && !hasType {
				t.Errorf("non-any union was widened to the empty schema: %s", raw)
			}
		})
	}
}

func TestBuildArgs_DeepDefaultMatchesOriginalJSON(t *testing.T) {
	var value any = "leaf"
	for i := 0; i < 10001; i++ {
		value = []any{value}
	}
	schema := map[string]any{"default": value}
	before, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("baseline marshal: %v", err)
	}
	if raw := outputSchemaArg(t, schema); raw != string(before) {
		t.Errorf("argv JSON differs from the baseline (got %d bytes, want %d)", len(raw), len(before))
	}
}

func anyJSONSchemaFixture() map[string]any {
	return map[string]any{
		"type": []any{"null", "string", "object", "boolean", "array", "number"},
	}
}

func outputSchemaArg(t *testing.T, schema map[string]any) string {
	t.Helper()
	args := buildArgs(processConfig{OutputFormat: schema}, false)
	raw := flagValue(args, "--json-schema")
	if raw == "" {
		t.Fatal("--json-schema missing from argv")
	}
	return raw
}

func countTypeUnions(value any) int {
	switch value := value.(type) {
	case map[string]any:
		count := 0
		if _, ok := value["type"].([]any); ok {
			count++
		}
		for _, nested := range value {
			count += countTypeUnions(nested)
		}
		return count
	case []any:
		count := 0
		for _, nested := range value {
			count += countTypeUnions(nested)
		}
		return count
	default:
		return 0
	}
}

func TestWithNoSettingSourcesIsDistinctFromNoOption(t *testing.T) {
	var c config
	WithNoSettingSources()(&c)
	if c.settingSources == nil || len(c.settingSources) != 0 {
		t.Fatalf("WithNoSettingSources set %#v, want an empty non-nil list", c.settingSources)
	}
	var unset config
	WithSettingSources()(&unset)
	if unset.settingSources != nil {
		t.Fatalf("WithSettingSources() with no source set %#v, want nil (CLI default)", unset.settingSources)
	}
}
