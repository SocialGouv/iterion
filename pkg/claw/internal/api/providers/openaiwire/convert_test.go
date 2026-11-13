package openaiwire

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

// Regression: a property whose declared JSON schema is the permissive
// empty form `{}` ("any value", e.g. iterion's `FieldTypeJSON`) must
// round-trip through Property as `{}`, not as `{"type": ""}`. OpenAI's
// function-schema validator rejects empty `type` with HTTP 400 (`” is
// not valid under any of the given schemas`), which surfaced in the
// claw structured-output recovery path when verdict schemas contained
// any-typed fields.
func TestConvertTools_AnyTypePropertyRoundTripsAsEmptySchema(t *testing.T) {
	rawSchema := []byte(`{
		"type": "object",
		"properties": {
			"approved": {"type": "boolean"},
			"blockers": {},
			"family":   {"type": "string", "enum": ["claude", "gpt"]}
		},
		"required": ["approved", "blockers", "family"]
	}`)

	var s api.InputSchema
	if err := json.Unmarshal(rawSchema, &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	tools, err := ConvertTools("openai", []api.Tool{{
		Name:        "structured_output",
		Description: "test",
		InputSchema: s,
	}})
	if err != nil {
		t.Fatalf("ConvertTools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}

	var compact bytes.Buffer
	if err := json.Compact(&compact, tools[0].Function.Parameters); err != nil {
		t.Fatal(err)
	}
	params := compact.String()
	if strings.Contains(params, `"type":""`) {
		t.Errorf("any-type property leaked empty `type`: %s", params)
	}
	if !strings.Contains(params, `"blockers":{}`) {
		t.Errorf("expected `\"blockers\":{}`, got: %s", params)
	}
	// The well-typed properties must still carry their type.
	if !strings.Contains(params, `"approved":{"type":"boolean"}`) {
		t.Errorf("approved lost its type: %s", params)
	}
	if !strings.Contains(params, `"family":{"type":"string"`) {
		t.Errorf("family lost its type: %s", params)
	}
}

// Sanity check: well-typed properties are preserved verbatim.
func TestConvertTools_WellTypedPropertyPreserved(t *testing.T) {
	tools, err := ConvertTools("openai", []api.Tool{{
		Name: "noop",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"name": {Type: "string"},
				"tags": {Type: "array", Items: &api.Property{Type: "string"}},
			},
			Required: []string{"name"},
		},
	}})
	if err != nil {
		t.Fatalf("ConvertTools: %v", err)
	}
	params := string(tools[0].Function.Parameters)
	if !strings.Contains(params, `"name":{"type":"string"}`) {
		t.Errorf("name lost its type: %s", params)
	}
	if !strings.Contains(params, `"tags":{"type":"array","items":{"type":"string"}}`) {
		t.Errorf("tags array.items malformed: %s", params)
	}
}
