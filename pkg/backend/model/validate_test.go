package model

import (
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestValidateOutput_StringArrayEnumEnforced guards the data-integrity
// fix that pushed the string[] [enum: ...] check into ValidateOutput.
// Before the fix, the generated JSON schema advertised the constraint
// to the LLM but server-side validation accepted any string value, so
// a stray entry flowed downstream unchecked.
func TestValidateOutput_StringArrayEnumEnforced(t *testing.T) {
	schema := &ir.Schema{
		Name: "out",
		Fields: []*ir.SchemaField{
			{
				Name:       "tags",
				Type:       ir.FieldTypeStringArray,
				EnumValues: []string{"red", "green", "blue"},
			},
		},
	}

	cases := []struct {
		name    string
		val     any
		wantErr string
	}{
		{
			name:    "rejects out-of-enum",
			val:     []any{"red", "purple"},
			wantErr: "not in enum",
		},
		{
			name: "accepts all-valid",
			val:  []any{"red", "green", "blue"},
		},
		{
			name: "accepts empty array",
			val:  []any{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateOutput(map[string]any{"tags": c.val}, schema)
			if c.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), c.wantErr)
			}
		})
	}
}

// TestConformComputeOutput covers #792: a compute node's evaluated output is
// conformed to its declared schema at the single place compute outputs are
// produced. The numeric REPRESENTATION is normalised — an integral float
// under `int` becomes an int64, an integer under `float` a float64 — and a
// fractional float under `int`, like every cross-kind mismatch, fails with
// the field named: the same rule ValidateOutput applies to an LLM's output,
// never a silent float under an `int` label.
func TestConformComputeOutput(t *testing.T) {
	schemaOf := func(typ ir.FieldType) *ir.Schema {
		return &ir.Schema{Name: "gauge", Fields: []*ir.SchemaField{{Name: "used_pct", Type: typ}}}
	}
	cases := []struct {
		name    string
		typ     ir.FieldType
		val     any
		want    any
		wantErr []string
	}{
		{"int keeps int64", ir.FieldTypeInt, int64(7), int64(7), nil},
		{"int normalises int", ir.FieldTypeInt, 7, int64(7), nil},
		{"int coerces an integral float", ir.FieldTypeInt, 3.0, int64(3), nil},
		{"int rejects a fractional float, naming the builtin", ir.FieldTypeInt, 10.58, nil, []string{`"used_pct"`, "10.58", "floor(", "round("}},
		{"int rejects a string", ir.FieldTypeInt, "7", nil, []string{`"used_pct"`, "expected integer", "string"}},
		{"int rejects a bool", ir.FieldTypeInt, true, nil, []string{"expected integer", "bool"}},
		{"float keeps float64", ir.FieldTypeFloat, 1.5, 1.5, nil},
		{"float promotes int64", ir.FieldTypeFloat, int64(7), 7.0, nil},
		{"float rejects a string", ir.FieldTypeFloat, "1.5", nil, []string{"expected number", "string"}},
		{"string keeps string", ir.FieldTypeString, "ok", "ok", nil},
		{"string rejects a number", ir.FieldTypeString, int64(7), nil, []string{"expected string", "int64"}},
		{"string rejects a bool", ir.FieldTypeString, true, nil, []string{"expected string", "bool"}},
		{"bool keeps bool", ir.FieldTypeBool, true, true, nil},
		{"bool rejects a string", ir.FieldTypeBool, "true", nil, []string{"expected bool", "string"}},
		{"bool rejects a number", ir.FieldTypeBool, int64(1), nil, []string{"expected bool", "int64"}},
		{"string[] keeps []any of strings", ir.FieldTypeStringArray, []any{"a", "b"}, []any{"a", "b"}, nil},
		{"string[] normalises []string", ir.FieldTypeStringArray, []string{"a"}, []any{"a"}, nil},
		{"string[] rejects a non-string element", ir.FieldTypeStringArray, []any{"a", int64(1)}, nil, []string{"expected string", "int64"}},
		{"json keeps anything", ir.FieldTypeJSON, map[string]any{"k": "v"}, map[string]any{"k": "v"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := map[string]any{"used_pct": c.val}
			err := ConformComputeOutput(out, schemaOf(c.typ))
			if len(c.wantErr) > 0 {
				if err == nil {
					t.Fatalf("expected an error naming %v, got nil — the value stayed %#v under a %s label", c.wantErr, out["used_pct"], c.typ)
				}
				for _, w := range c.wantErr {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("error %q does not name %q", err, w)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(out["used_pct"], c.want) {
				t.Errorf("value after conformance = %#v (%T), want %#v (%T)", out["used_pct"], out["used_pct"], c.want, c.want)
			}
		})
	}
}

// A field the expression left nil, or one the schema declares that no
// expression produced, is the ABSENCE class, not a type mismatch; the rule
// conforms the values that are present and leaves those alone.
func TestConformComputeOutput_LeavesAbsentAndNilAlone(t *testing.T) {
	schema := &ir.Schema{Name: "s", Fields: []*ir.SchemaField{
		{Name: "a", Type: ir.FieldTypeInt},
		{Name: "b", Type: ir.FieldTypeString},
	}}
	out := map[string]any{"a": nil}
	if err := ConformComputeOutput(out, schema); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v, ok := out["a"]; !ok || v != nil {
		t.Errorf("nil value was rewritten to %#v", v)
	}
	if _, ok := out["b"]; ok {
		t.Error("an absent field was invented")
	}
}

// The LLM path and the compute rule agree on what an integer is: a Go
// integer under `int` (or under `float`) is a number of the right kind, not
// a type error — ValidateOutput used to accept float64 alone, the JSON
// decoder's shape, and reject the int64 a compute or a subbot produces.
func TestValidateOutput_NumericKindsAgreeWithCompute(t *testing.T) {
	intSchema := &ir.Schema{Name: "s", Fields: []*ir.SchemaField{{Name: "n", Type: ir.FieldTypeInt}}}
	for _, v := range []any{int64(3), 3, 3.0} {
		if err := ValidateOutput(map[string]any{"n": v}, intSchema); err != nil {
			t.Errorf("int field rejected %#v (%T): %v", v, v, err)
		}
	}
	if err := ValidateOutput(map[string]any{"n": 3.5}, intSchema); err == nil || !strings.Contains(err.Error(), "expected integer") {
		t.Errorf("int field accepted a fractional float: %v", err)
	}
	floatSchema := &ir.Schema{Name: "s", Fields: []*ir.SchemaField{{Name: "x", Type: ir.FieldTypeFloat}}}
	for _, v := range []any{1.5, int64(2), 2} {
		if err := ValidateOutput(map[string]any{"x": v}, floatSchema); err != nil {
			t.Errorf("float field rejected %#v (%T): %v", v, v, err)
		}
	}
}
