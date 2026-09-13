package model

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func TestPublicSchemaPreservesRequirednessAndArrayItemShape(t *testing.T) {
	object := &ir.Schema{Name: "Record", Fields: []*ir.SchemaField{{Name: "title", Type: ir.FieldTypeString}}}
	array, err := ir.ResolvePortType("Record[]", map[string]*ir.Schema{"Record": object})
	if err != nil {
		t.Fatal(err)
	}
	text, _ := ir.ResolvePortType("string", nil)
	min := 0
	schema := &ir.Schema{NativePorts: true, PublicPorts: []ir.PublicPort{
		{Name: "records", Type: array, Required: true, MinItems: &min},
		{Name: "note", Type: text, Required: false, Nullable: true},
	}}
	raw, err := SchemaToJSON(schema)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(wire["required"], []any{"records"}) {
		t.Fatalf("optional note became required: %s", raw)
	}
	properties := wire["properties"].(map[string]any)
	records := properties["records"].(map[string]any)
	if records["type"] != "array" || records["items"].(map[string]any)["type"] != "object" {
		t.Fatalf("nested array schema was erased: %s", raw)
	}
	if _, nullable := properties["note"].(map[string]any)["anyOf"]; !nullable {
		t.Fatalf("nullability was erased: %s", raw)
	}
	for _, output := range []map[string]any{
		{"records": []any{}},
		{"records": []any{map[string]any{"title": "valid"}}, "note": nil},
	} {
		if err := ValidateOutput(output, schema); err != nil {
			t.Fatal(err)
		}
	}
	if err := ValidateOutput(map[string]any{"records": []any{map[string]any{"missing": "title"}}}, schema); err == nil {
		t.Fatal("resolved object requirements were not checked")
	}
}

func TestPublicComputeValidationDoesNotCoerceValues(t *testing.T) {
	integer, _ := ir.ResolvePortType("int", nil)
	schema := &ir.Schema{NativePorts: true, PublicPorts: []ir.PublicPort{{Name: "value", Type: integer, Required: true}}}
	output := map[string]any{"value": json.Number("9007199254740993")}
	if err := ConformComputeOutput(output, schema); err != nil {
		t.Fatal(err)
	}
	if _, preserved := output["value"].(json.Number); !preserved {
		t.Fatal("native compute coerced its public output representation")
	}
	if err := ConformComputeOutput(map[string]any{"value": "42"}, schema); err == nil {
		t.Fatal("a string was implicitly converted to an integer")
	}
}
