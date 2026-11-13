package api

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestSchemaRawRoundTrip(t *testing.T) {
	for _, input := range []string{
		`{"additionalProperties":false,"type":"object","properties":{"n":{"type":"integer","minimum":0,"maximum":100}},"required":["n"]}`,
		`{"properties":{"allow":true,"deny":false,"tuple":{"type":"array","prefixItems":[{"type":"string"},{"type":"integer"}],"items":false}}}`,
		`{"type":["object","null"],"$defs":{"node":{"$ref":"#/$defs/node"}},"unevaluatedProperties":false}`,
		`{"enum":[9007199254740993,1.20e+3],"pattern":"\\u0041"}`,
		`{}`, `true`, `false`,
	} {
		t.Run(input, func(t *testing.T) {
			for _, value := range []any{&InputSchema{}, &Property{}} {
				if err := json.Unmarshal([]byte(input), value); err != nil {
					t.Fatal(err)
				}
				got, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != input {
					t.Errorf("%T round-trip = %s, want %s", value, got, input)
				}
			}
		})
	}
}

func TestSchemaRawTypedViewAndOwnership(t *testing.T) {
	input := []byte(`{"type":"object","properties":{"rows":{"type":"array","items":{"type":["null","integer"],"minimum":0}}},"required":["rows"],"additionalProperties":false}`)
	want := bytes.Clone(input)
	var schema InputSchema
	if err := json.Unmarshal(input, &schema); err != nil {
		t.Fatal(err)
	}
	rows := schema.Properties["rows"]
	if schema.Type != "object" || len(schema.Required) != 1 || rows.Type != "array" || rows.Items == nil || rows.Items.Type != "integer" || len(rows.Items.Types) != 2 {
		t.Fatalf("typed accessors lost: %+v, rows: %+v", schema, rows)
	}
	input[0] = '!'
	schema.Type = "array"
	got, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("raw must own its bytes and take precedence over the read view: %s", got)
	}
	if err := json.Unmarshal([]byte(`{}`), &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Type != "" || schema.Properties != nil || schema.Required != nil || string(schema.Raw) != "{}" {
		t.Fatalf("reusing a schema retains stale fields: %+v", schema)
	}
}

func TestSchemaRawInTypedParent(t *testing.T) {
	var child Property
	if err := json.Unmarshal([]byte(`{"minimum":2,"type":"number","x-custom":true}`), &child); err != nil {
		t.Fatal(err)
	}
	parent := InputSchema{Type: "object", Properties: map[string]Property{"n": child}}
	got, err := json.Marshal(parent)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte(`"n":{"minimum":2,"type":"number","x-custom":true}`)) {
		t.Fatalf("typed parent loses raw child: %s", got)
	}
}
