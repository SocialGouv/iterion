package delegate

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestCodexTransportSchemaEncodesOnlyAnyJSONFields(t *testing.T) {
	canonical := json.RawMessage(`{
		"type": "object",
		"properties": {
			"status": {"type": "string", "enum": ["ok", "failed"]},
			"payload": {"type": ["null", "boolean", "number", "string", "array", "object"]},
			"metadata": {
				"type": ["object", "array", "string", "number", "boolean", "null"],
				"description": "Original field guidance."
			},
			"closed_object": {
				"type": "object",
				"properties": {"id": {"type": "string"}},
				"required": ["id"],
				"additionalProperties": false
			},
			"partial_union": {"type": ["object", "array", "string"]}
		},
		"required": ["status", "payload", "metadata", "closed_object", "partial_union"],
		"additionalProperties": false
	}`)

	transport, fields, err := codexTransportSchema(canonical)
	if err != nil {
		t.Fatalf("codexTransportSchema: %v", err)
	}
	if !reflect.DeepEqual(fields, []string{"metadata", "payload"}) {
		t.Fatalf("JSON transport fields = %v, want [metadata payload]", fields)
	}

	var schema map[string]any
	if err := json.Unmarshal(transport, &schema); err != nil {
		t.Fatalf("transport schema is invalid JSON: %v", err)
	}
	properties := schema["properties"].(map[string]any)

	for _, name := range fields {
		property := properties[name].(map[string]any)
		if property["type"] != "string" {
			t.Errorf("%s type = %v, want string", name, property["type"])
		}
		description, _ := property["description"].(string)
		if !strings.Contains(description, "JSON-encoded string") {
			t.Errorf("%s description does not explain transport: %q", name, description)
		}
	}
	metadataDescription := properties["metadata"].(map[string]any)["description"].(string)
	if !strings.HasPrefix(metadataDescription, "Original field guidance. ") {
		t.Errorf("existing description was not preserved: %q", metadataDescription)
	}

	if got := properties["status"].(map[string]any)["type"]; got != "string" {
		t.Errorf("ordinary string field changed: %v", got)
	}
	if got := properties["closed_object"].(map[string]any)["type"]; got != "object" {
		t.Errorf("closed object field changed: %v", got)
	}
	partialType := properties["partial_union"].(map[string]any)["type"]
	if !reflect.DeepEqual(partialType, []any{"object", "array", "string"}) {
		t.Errorf("non-FieldTypeJSON union changed: %v", partialType)
	}
	if schema["additionalProperties"] != false {
		t.Errorf("root additionalProperties = %v, want false", schema["additionalProperties"])
	}
	if got := schema["required"].([]any); len(got) != 5 {
		t.Errorf("required fields changed: %v", got)
	}
}

func TestCodexTransportSchemaNoJSONFieldIsBytePreserving(t *testing.T) {
	canonical := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`)

	transport, fields, err := codexTransportSchema(canonical)
	if err != nil {
		t.Fatalf("codexTransportSchema: %v", err)
	}
	if len(fields) != 0 {
		t.Fatalf("unexpected JSON transport fields: %v", fields)
	}
	if string(transport) != string(canonical) {
		t.Fatalf("schema without FieldTypeJSON was rewritten:\n got: %s\nwant: %s", transport, canonical)
	}
}

func TestCodexTransportSchemaRejectsMalformedSchema(t *testing.T) {
	tests := []struct {
		name   string
		schema json.RawMessage
	}{
		{name: "invalid JSON", schema: json.RawMessage(`{"type":`)},
		{name: "properties is not object", schema: json.RawMessage(`{"type":"object","properties":[]}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := codexTransportSchema(test.schema); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestIsAnyJSONTypeUnionRequiresExactSet(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  bool
	}{
		{
			name:  "canonical order",
			value: []any{"object", "array", "string", "number", "boolean", "null"},
			want:  true,
		},
		{
			name:  "permuted order",
			value: []any{"null", "string", "object", "boolean", "array", "number"},
			want:  true,
		},
		{
			name:  "missing kind",
			value: []any{"object", "array", "string", "number", "boolean"},
			want:  false,
		},
		{
			name:  "duplicate replacing kind",
			value: []any{"object", "array", "string", "number", "boolean", "boolean"},
			want:  false,
		},
		{
			name:  "extra unknown kind",
			value: []any{"object", "array", "string", "number", "boolean", "null", "integer"},
			want:  false,
		},
		{
			name:  "scalar type",
			value: "object",
			want:  false,
		},
		{
			name:  "non-string member",
			value: []any{"object", "array", "string", "number", "boolean", nil},
			want:  false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isAnyJSONTypeUnion(test.value); got != test.want {
				t.Fatalf("isAnyJSONTypeUnion(%v) = %v, want %v", test.value, got, test.want)
			}
		})
	}
}

func TestDecodeCodexJSONTransportAllJSONKinds(t *testing.T) {
	tests := []struct {
		name    string
		encoded string
		want    any
	}{
		{
			name:    "object",
			encoded: `{"name":"town","count":2}`,
			want:    map[string]any{"name": "town", "count": float64(2)},
		},
		{
			name:    "array",
			encoded: `[{"id":1},true,null]`,
			want:    []any{map[string]any{"id": float64(1)}, true, nil},
		},
		{name: "string scalar", encoded: `"ready"`, want: "ready"},
		{name: "number scalar", encoded: `42.5`, want: float64(42.5)},
		{name: "boolean scalar", encoded: `false`, want: false},
		{name: "null", encoded: `null`, want: nil},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output := map[string]any{
				"payload":   test.encoded,
				"untouched": "same",
			}
			if err := decodeCodexJSONTransport(output, []string{"payload"}); err != nil {
				t.Fatalf("decodeCodexJSONTransport: %v", err)
			}
			got, present := output["payload"]
			if !present {
				t.Fatal("decoded field disappeared")
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("decoded value = %#v (%T), want %#v (%T)", got, got, test.want, test.want)
			}
			if output["untouched"] != "same" {
				t.Errorf("non-JSON field changed: %#v", output["untouched"])
			}
		})
	}
}

func TestDecodeCodexJSONTransportRejectsMalformedAtomically(t *testing.T) {
	output := map[string]any{
		"first":  `[1,2]`,
		"second": `{"unterminated":`,
	}

	err := decodeCodexJSONTransport(output, []string{"first", "second"})
	if err == nil || !strings.Contains(err.Error(), `field "second": invalid encoded JSON`) {
		t.Fatalf("error = %v, want malformed second-field error", err)
	}
	if output["first"] != `[1,2]` || output["second"] != `{"unterminated":` {
		t.Fatalf("decode was not atomic; output mutated on error: %#v", output)
	}
}

func TestDecodeCodexJSONTransportRejectsNonString(t *testing.T) {
	output := map[string]any{"payload": map[string]any{"already": "decoded"}}

	err := decodeCodexJSONTransport(output, []string{"payload"})
	if err == nil || !strings.Contains(err.Error(), "expected JSON-encoded string") {
		t.Fatalf("error = %v, want non-string transport error", err)
	}
}

func TestDecodeCodexJSONTransportLeavesMissingFieldForValidator(t *testing.T) {
	output := map[string]any{"status": "ok"}
	if err := decodeCodexJSONTransport(output, []string{"payload"}); err != nil {
		t.Fatalf("missing transport field should be handled by schema validation, got: %v", err)
	}
	if !reflect.DeepEqual(output, map[string]any{"status": "ok"}) {
		t.Fatalf("output changed: %#v", output)
	}
}
