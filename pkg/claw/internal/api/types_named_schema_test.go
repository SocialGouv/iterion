package api

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestInputSchemaNamedDefinitionsRoundTrip(t *testing.T) {
	raw, err := os.ReadFile("testdata/named_schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema InputSchema
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("named schema changed through InputSchema: got %s; want %s", encoded, raw)
	}
}

func TestInputSchemaWithoutDefinitionsKeepsWire(t *testing.T) {
	for _, raw := range []string{
		`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`,
		`{"type":"object","properties":{"anything":{}}}`,
	} {
		var schema InputSchema
		if err := json.Unmarshal([]byte(raw), &schema); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(schema)
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != raw {
			t.Fatalf("flat schema wire changed: got %s; want %s", encoded, raw)
		}
	}
}
