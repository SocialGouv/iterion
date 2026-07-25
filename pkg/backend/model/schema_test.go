package model

import (
	"encoding/json"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestSchemaToJSON_JSONFieldIsAnyType pins the JSON-schema shape of a
// FieldTypeJSON property against two live bugs that pull in opposite
// directions:
//
//  1. A single `"type": "object"` rejected every non-object shape. Seen
//     on secured-renovacy/main.bot run_1778786106222 (sonnet+high) and
//     run_1778784391171 (opus+max): detect_stack populated the recipe's
//     `ecosystems: json` field as a JSON array (the only sensible shape
//     for "list of per-ecosystem profiles"), the formatter's derived
//     schema declared it `{"type": "object"}`, JSON Schema rejected the
//     array, and the value was stripped to nothing → `raw_output_len: 0`
//     + "missing required field ecosystems".
//  2. The opposite fix — the empty schema `{}` (no "type" key at all) —
//     is rejected by OpenAI/codex's structured-output formatting pass
//     with `invalid_json_schema: In context=('properties', <field>),
//     schema must have a 'type' key`, failing the whole node (surfacing
//     as `codex formatting pass returned empty structured output`).
//
// FieldTypeJSON's contract is "accepts any value", so the property must
// carry a "type" *union* over every JSON kind: the "type" key satisfies
// providers that require one (bug 2), and every JSON shape stays valid
// (bug 1).
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
	// Bug 2: the property MUST declare a "type" key (codex/OpenAI reject
	// a type-less property).
	rawType, hasType := eco["type"]
	if !hasType {
		t.Fatalf("ecosystems (FieldTypeJSON) must declare a JSON-schema 'type' key — codex/OpenAI's formatting pass 400s on a type-less property (invalid_json_schema). got: %s", raw)
	}
	// Bug 1: the type must be a union that still admits arrays (and every
	// other JSON kind) — never a single scalar type that would reject them.
	typeList, ok := rawType.([]any)
	if !ok {
		t.Fatalf("ecosystems type must be a union list over all JSON kinds, not a single type (a single type re-introduces the array-rejection bug). got type=%v in: %s", rawType, raw)
	}
	got := make(map[string]bool, len(typeList))
	for _, v := range typeList {
		if s, ok := v.(string); ok {
			got[s] = true
		}
	}
	for _, want := range []string{"object", "array", "string", "number", "boolean", "null"} {
		if !got[want] {
			t.Errorf("ecosystems type union missing %q — FieldTypeJSON must accept every JSON kind. got=%v", want, typeList)
		}
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
