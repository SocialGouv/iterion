package api

import (
	"encoding/json"
	"testing"
)

// A schema generator that has no single kind for a field spells "any" as
// the union of every JSON kind. That is valid JSON Schema and must parse.
func TestProperty_TypeArrayRoundTrip(t *testing.T) {
	in := `{"type":"object","properties":{"voter_id":{"type":"string"},"verdicts":{"type":["object","array","string","number","boolean","null"]}},"required":["voter_id","verdicts"]}`
	var schema InputSchema
	if err := json.Unmarshal([]byte(in), &schema); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	v := schema.Properties["verdicts"]
	if v.Type != "object" {
		t.Errorf("scalar view = %q, want the first non-null kind %q", v.Type, "object")
	}
	if len(v.Types) != 6 {
		t.Errorf("Types = %v, want the six kinds", v.Types)
	}
	if s := schema.Properties["voter_id"]; s.Type != "string" || s.Types != nil {
		t.Errorf("single-type property changed: %+v", s)
	}
	out, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back InputSchema
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("re-unmarshal %s: %v", out, err)
	}
	if got := back.Properties["verdicts"].Types; len(got) != 6 {
		t.Errorf("array form did not round-trip: %s", out)
	}
	if got := back.Properties["voter_id"]; got.Type != "string" {
		t.Errorf("string form did not round-trip: %s", out)
	}
}

func TestProperty_TypeArrayNullFirst(t *testing.T) {
	var p Property
	if err := json.Unmarshal([]byte(`{"type":["null","integer"]}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.Type != "integer" {
		t.Errorf("scalar view = %q, want %q", p.Type, "integer")
	}
	if err := json.Unmarshal([]byte(`{"type":["null"]}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.Type != "" || len(p.Types) != 1 {
		t.Errorf("all-null: %+v", p)
	}
}

func TestProperty_TypeNested(t *testing.T) {
	in := `{"type":"array","items":{"type":["string","null"]},"properties":{"n":{"type":["number","null"]}}}`
	var p Property
	if err := json.Unmarshal([]byte(in), &p); err != nil {
		t.Fatal(err)
	}
	if p.Items == nil || p.Items.Type != "string" || len(p.Items.Types) != 2 {
		t.Errorf("items: %+v", p.Items)
	}
	if n := p.Properties["n"]; n.Type != "number" {
		t.Errorf("nested property: %+v", n)
	}
}

func TestProperty_TypeInvalid(t *testing.T) {
	for _, in := range []string{`{"type":5}`, `{"type":[1,2]}`, `{"type":{"a":1}}`} {
		var p Property
		if err := json.Unmarshal([]byte(in), &p); err != nil {
			t.Fatal(err)
		}
		if string(p.Raw) != "{}" {
			t.Errorf("invalid type must be omitted: %s", p.Raw)
		}
	}
}

func TestProperty_AnyValueStaysEmpty(t *testing.T) {
	var p Property
	if err := json.Unmarshal([]byte(`{}`), &p); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(p)
	if string(out) != `{}` {
		t.Errorf("any-value property must round-trip as {}, got %s", out)
	}
}
