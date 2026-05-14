package model

import (
	"encoding/json"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestSchemaToJSON_JSONFieldIsAnyType is a regression for a live bug seen
// on secured-renovacy.iter run_1778786106222 (sonnet+high) and
// run_1778784391171 (opus+max). detect_stack populated the recipe's
// `ecosystems: json` field as a JSON array (the only sensible shape for
// "list of per-ecosystem profiles"). The model-formatter pass invoked
// `claude --json-schema <derived>` to normalise the agent's response;
// the derived schema declared `"ecosystems": {"type": "object"}`, which
// JSON Schema interprets as "must be a JSON object — arrays REJECTED".
// The formatter stripped the agent's array-shaped output to nothing,
// surfacing as `raw_output_len: 0` + a "missing required field
// ecosystems / primary_ecosystem_id / pkg_manager / …" validation error
// (the validator now seeing an empty `{}`).
//
// FieldTypeJSON's contract is "accepts any value", so the JSON Schema
// for that property must be the empty schema `{}` — which JSON Schema
// treats as "any value of any type". Empty schema with `additional
// Properties: false` on the parent object still allows the property to
// be present with any contents.
func TestSchemaToJSON_JSONFieldIsAnyType(t *testing.T) {
	schema := &ir.Schema{
		Name: "stack_profile",
		Fields: []*ir.SchemaField{
			{Name: "ecosystems", Type: ir.FieldTypeJSON},
			{Name: "primary_ecosystem_id", Type: ir.FieldTypeString},
		},
	}

	raw, err := SchemaToJSON(schema)
	if err != nil {
		t.Fatalf("SchemaToJSON: %v", err)
	}

	var parsed struct {
		Properties map[string]map[string]any `json:"properties"`
		Required   []string                  `json:"required"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	eco, ok := parsed.Properties["ecosystems"]
	if !ok {
		t.Fatalf("ecosystems property missing from generated schema: %s", raw)
	}
	if typ, hasType := eco["type"]; hasType {
		t.Fatalf("ecosystems (FieldTypeJSON) must NOT declare a JSON-schema type — doing so constrains the value to one JSON primitive type and rejects the others (the live bug rejected arrays). got type=%v in: %s", typ, raw)
	}

	// Sanity: a typed field still gets its type.
	pe, ok := parsed.Properties["primary_ecosystem_id"]
	if !ok {
		t.Fatalf("primary_ecosystem_id property missing")
	}
	if pe["type"] != "string" {
		t.Errorf("primary_ecosystem_id type = %v, want string", pe["type"])
	}

	// All declared fields stay required (so a forgetful agent still
	// gets a "missing required field" error rather than silently
	// emitting a partial object).
	if len(parsed.Required) != 2 {
		t.Errorf("required length = %d, want 2: %v", len(parsed.Required), parsed.Required)
	}
}
