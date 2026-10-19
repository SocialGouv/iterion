package claudesdk

import (
	"encoding/json"
	"strings"
	"testing"
)

// The rgaa-audit publish judge (#2500) is the shape this exists for: a
// structured output whose `json` fields materialize as a type union, which
// the CLI's strict validator refuses — silently stripping the whole
// structured output.
func TestStrictStructuredOutput(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "created": {"type": ["array", "object", "string", "number", "boolean", "null"]},
	    "existing": {"anyOf": ["array", "object", "string", "number", "boolean", "null"]},
	    "summary": {"type": "string"},
	    "nested": {"type": "object", "properties": {"payload": {"type": ["object", "null"]}}}
	  },
	  "required": ["created", "existing", "failed", "summary"],
	  "additionalProperties": false
	}`), &doc); err != nil {
		t.Fatal(err)
	}
	out := strictStructuredOutput(doc)
	for _, want := range []string{
		`"created":{}`,
		`"summary":{"type":"string"}`,
		`"payload":{}`,
		`"required":["created","existing","failed","summary"]`,
		`"additionalProperties":false`,
	} {
		if !strings.Contains(mustJSON(out), want) {
			t.Fatalf("the strict schema lost %s:\n%s", want, mustJSON(out))
		}
	}
	for _, gone := range []string{`"type":["array"`, `"anyOf":["array"`, `"type":["object","null"`} {
		if strings.Contains(mustJSON(out), gone) {
			t.Fatalf("the strict schema still carries the union %s:\n%s", gone, mustJSON(out))
		}
	}
	// The sanitizer mutates in place: the same document, sanitized twice,
	// is the schema once.
	again := strictStructuredOutput(doc)
	if mustJSON(again) != mustJSON(out) {
		t.Fatalf("a second pass changed the schema:\n%s", mustJSON(again))
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// The buildArgs boundary is where the sanitization must hold: every
// claude_code node with a structured output passes through here, and a
// regression at the call site hands the CLI the raw union again.
func TestBuildArgs_StrictStructuredOutput(t *testing.T) {
	args := buildArgs(processConfig{
		OutputFormat: map[string]any{
			"type": "object",
			"properties": map[string]any{
				// As a decoded JSON document carries it: []any, not []string.
				"created": map[string]any{"type": []any{"array", "object", "string", "number", "boolean", "null"}},
				"summary": map[string]any{"type": "string"},
			},
			"required":             []string{"created", "summary"},
			"additionalProperties": false,
		},
	}, true)
	schema := flagValue(args, "--json-schema")
	if schema == "" {
		t.Fatal("buildArgs emitted no --json-schema for a structured output")
	}
	for _, gone := range []string{`"type":["array"`, `"type": ["array"`} {
		if strings.Contains(schema, gone) {
			t.Fatalf("the CLI schema still carries the union %s:\n%s", gone, schema)
		}
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(schema), &doc); err != nil {
		t.Fatalf("the emitted schema is not JSON: %v\n%s", err, schema)
	}
	props := doc["properties"].(map[string]any)
	if len(props["created"].(map[string]any)) != 0 {
		t.Fatalf("the json field is not the type-less any-schema: %v", props["created"])
	}
	if props["summary"].(map[string]any)["type"] != "string" {
		t.Fatalf("the string field lost its type: %v", props["summary"])
	}
}
